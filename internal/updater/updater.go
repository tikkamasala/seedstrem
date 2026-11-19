// Package updater replaces the running seedstrem binary with the newest
// build published on the GitHub "edge" release (rebuilt by CI on every
// push to main).
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// cacheTTL bounds GitHub API calls: unauthenticated requests are limited
// to 60/h per IP, and a shared seedbox shares its IP with other users.
const cacheTTL = 15 * time.Minute

// Info is what the UI needs to offer an update.
type Info struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
}

// Updater checks for and installs new builds.
type Updater struct {
	Repo    string // owner/name
	Tag     string // release tag to track
	APIBase string
	Current string // running version
	Exe     string // path of the running binary
	HTTP    *http.Client

	mu        sync.Mutex
	cached    Info
	cachedAt  time.Time
	applyLock sync.Mutex
}

// New returns an updater for the running binary. The executable path is
// resolved now: once the file is renamed over, /proc/self/exe no longer
// points at the installed path.
func New(current string) *Updater {
	exe, err := os.Executable()
	if err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
	}
	return &Updater{
		Repo:    "tikkamasala/seedstrem",
		Tag:     "edge",
		APIBase: "https://api.github.com",
		Current: current,
		Exe:     exe,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// unsupported reports why self-update can't work here, if it can't.
func (u *Updater) unsupported() string {
	if runtime.GOOS != "linux" {
		return "self-update only supports linux builds"
	}
	if u.Exe == "" {
		return "cannot locate the running binary"
	}
	f, err := os.CreateTemp(filepath.Dir(u.Exe), ".seedstrem-probe-*")
	if err != nil {
		return "binary directory is not writable (in Docker, pull the new image instead)"
	}
	f.Close()
	os.Remove(f.Name())
	return ""
}

// Check reports whether a newer build is published. Results are cached.
func (u *Updater) Check(ctx context.Context) (Info, error) {
	if reason := u.unsupported(); reason != "" {
		return Info{Reason: reason, Current: u.Current}, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.cachedAt.IsZero() && time.Since(u.cachedAt) < cacheTTL {
		return u.cached, nil
	}
	rel, err := u.release(ctx)
	if err != nil {
		return Info{}, err
	}
	latest, err := u.latestVersion(ctx, rel)
	if err != nil {
		return Info{}, err
	}
	u.cached = Info{Supported: true, Current: u.Current, Latest: latest, Available: latest != u.Current}
	u.cachedAt = time.Now()
	return u.cached, nil
}

// Apply downloads the published binary for this architecture, verifies
// its checksum and renames it over the running binary. The caller must
// restart the process to run it. Returns the installed version.
func (u *Updater) Apply(ctx context.Context) (string, error) {
	if reason := u.unsupported(); reason != "" {
		return "", errors.New(reason)
	}
	u.applyLock.Lock()
	defer u.applyLock.Unlock()

	rel, err := u.release(ctx)
	if err != nil {
		return "", err
	}
	latest, err := u.latestVersion(ctx, rel)
	if err != nil {
		return "", err
	}
	name := "seedstrem_linux_" + runtime.GOARCH
	want, err := u.checksum(ctx, rel, name)
	if err != nil {
		return "", err
	}
	binURL, ok := rel.asset(name)
	if !ok {
		return "", fmt.Errorf("release %s has no %s asset", u.Tag, name)
	}
	body, err := u.get(ctx, binURL)
	if err != nil {
		return "", err
	}
	defer body.Close()

	tmp := filepath.Join(filepath.Dir(u.Exe), ".seedstrem.new")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch for %s", name)
	}
	// Rename rather than overwrite so the running binary isn't "text file busy".
	if err := os.Rename(tmp, u.Exe); err != nil {
		return "", err
	}
	u.mu.Lock()
	u.cachedAt = time.Time{}
	u.mu.Unlock()
	return latest, nil
}

type release struct {
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r release) asset(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, true
		}
	}
	return "", false
}

func (u *Updater) release(ctx context.Context) (release, error) {
	var rel release
	body, err := u.get(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", u.APIBase, u.Repo, u.Tag))
	if err != nil {
		return rel, err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(&rel); err != nil {
		return rel, fmt.Errorf("decode release: %w", err)
	}
	return rel, nil
}

func (u *Updater) latestVersion(ctx context.Context, rel release) (string, error) {
	b, err := u.small(ctx, rel, "version.txt")
	return strings.TrimSpace(b), err
}

func (u *Updater) checksum(ctx context.Context, rel release, name string) (string, error) {
	sums, err := u.small(ctx, rel, "checksums.txt")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// small fetches a small text asset.
func (u *Updater) small(ctx context.Context, rel release, name string) (string, error) {
	url, ok := rel.asset(name)
	if !ok {
		return "", fmt.Errorf("release %s has no %s asset", u.Tag, name)
	}
	body, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 64<<10))
	return string(b), err
}

func (u *Updater) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "seedstrem-updater")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

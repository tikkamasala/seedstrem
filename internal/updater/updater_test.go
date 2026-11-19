package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeGitHub serves an edge release with version.txt, checksums.txt and a
// binary for the test's GOARCH.
func fakeGitHub(t *testing.T, version, bin, sum string) *httptest.Server {
	t.Helper()
	name := "seedstrem_linux_" + runtime.GOARCH
	if sum == "" {
		h := sha256.Sum256([]byte(bin))
		sum = hex.EncodeToString(h[:])
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/tags/edge", func(w http.ResponseWriter, _ *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		json.NewEncoder(w).Encode(map[string]any{"assets": []asset{
			{"version.txt", srv.URL + "/dl/version.txt"},
			{"checksums.txt", srv.URL + "/dl/checksums.txt"},
			{name, srv.URL + "/dl/bin"},
		}})
	})
	mux.HandleFunc("/dl/version.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(version + "\n")) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(sum + "  " + name + "\n"))
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(bin)) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestUpdater(t *testing.T, srv *httptest.Server, current string) *Updater {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("self-update is linux-only")
	}
	exe := filepath.Join(t.TempDir(), "seedstrem")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := New(current)
	u.Repo, u.APIBase, u.Exe, u.HTTP = "o/r", srv.URL, exe, srv.Client()
	return u
}

func TestCheck(t *testing.T) {
	srv := fakeGitHub(t, "v2", "new", "")
	for _, tc := range []struct {
		current string
		want    bool
	}{{"v1", true}, {"v2", false}} {
		info, err := newTestUpdater(t, srv, tc.current).Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !info.Supported || info.Latest != "v2" || info.Available != tc.want {
			t.Errorf("current %s: got %+v", tc.current, info)
		}
	}
}

func TestApplyReplacesBinary(t *testing.T) {
	u := newTestUpdater(t, fakeGitHub(t, "v2", "new", ""), "v1")
	v, err := u.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(u.Exe)
	if v != "v2" || string(got) != "new" {
		t.Fatalf("version %q, binary %q", v, got)
	}
	if fi, _ := os.Stat(u.Exe); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary not executable: %v", fi.Mode())
	}
}

func TestApplyChecksumMismatch(t *testing.T) {
	u := newTestUpdater(t, fakeGitHub(t, "v2", "new", "deadbeef"), "v1")
	if _, err := u.Apply(context.Background()); err == nil {
		t.Fatal("expected checksum error")
	}
	got, _ := os.ReadFile(u.Exe)
	if string(got) != "old" {
		t.Fatalf("binary changed to %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(u.Exe), ".seedstrem.new")); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestUnwritableDirUnsupported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	u := newTestUpdater(t, fakeGitHub(t, "v2", "new", ""), "v1")
	dir := filepath.Dir(u.Exe)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	info, err := u.Check(context.Background())
	if err != nil || info.Supported || info.Reason == "" {
		t.Fatalf("got %+v, %v", info, err)
	}
}

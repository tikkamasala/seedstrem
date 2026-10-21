package rarvol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// HeaderProbe is how many leading bytes of a volume Parse reads. A stored
// volume's headers (marker, archive header, file header with its name)
// fit in a few hundred bytes; the margin covers comments and long names.
const HeaderProbe = 16 << 10

var (
	ErrNotRar     = errors.New("not a RAR volume")
	ErrCompressed = errors.New("RAR volume is compressed, only stored (-m0) archives can be streamed")
	ErrEncrypted  = errors.New("RAR volume is encrypted")
	ErrNoFile     = errors.New("RAR volume holds no file")
)

var (
	marker4 = []byte("Rar!\x1a\x07\x00")
	marker5 = []byte("Rar!\x1a\x07\x01\x00")
)

// Header is what one volume says about the file data it carries.
type Header struct {
	Format       int    // 4 or 5
	InnerName    string // path inside the archive, slash-separated
	DataOffset   int64  // where this volume's data block starts
	PackSize     int64  // length of this volume's data block
	UnpackedSize int64  // size of the whole inner file; 0 when unknown
	// mainHeaderSize is the RAR5 archive header's size field: it grows
	// with the volume-number field, which Build accounts for.
	mainHeaderSize uint64
}

// Parse reads the headers at the start of a volume of the given size and
// returns the first file's data block. Only stored, unencrypted files
// are accepted.
func Parse(r io.ReaderAt, size int64) (Header, error) {
	n := min(size, HeaderProbe)
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return Header{}, fmt.Errorf("read volume header: %w", err)
	}
	switch {
	case bytes.HasPrefix(buf, marker5):
		return parse5(buf)
	case bytes.HasPrefix(buf, marker4):
		return parse4(buf)
	}
	return Header{}, ErrNotRar
}

var errTruncated = fmt.Errorf("%w: headers truncated", ErrNotRar)

// parse4 walks RAR 1.5–4.x blocks: CRC16, type, flags, size, then an
// optional 32-bit ADD_SIZE of data following the block.
func parse4(buf []byte) (Header, error) {
	le16 := func(p int) int { return int(binary.LittleEndian.Uint16(buf[p:])) }
	le32 := func(p int) int64 { return int64(binary.LittleEndian.Uint32(buf[p:])) }
	pos := len(marker4)
	for {
		if pos+7 > len(buf) {
			return Header{}, errTruncated
		}
		typ, flags, hsize := buf[pos+2], le16(pos+3), le16(pos+5)
		if hsize < 7 || pos+hsize > len(buf) {
			return Header{}, errTruncated
		}
		switch typ {
		case 0x73: // archive header
			if flags&0x0080 != 0 { // block headers encrypted
				return Header{}, ErrEncrypted
			}
			pos += hsize
		case 0x74: // file header
			if hsize < 32 {
				return Header{}, errTruncated
			}
			pack, unp := le32(pos+7), le32(pos+11)
			method, nameSize := buf[pos+25], le16(pos+26)
			nameAt := pos + 32
			if flags&0x0100 != 0 { // 64-bit sizes
				if hsize < 40 {
					return Header{}, errTruncated
				}
				pack |= le32(pos+32) << 32
				unp |= le32(pos+36) << 32
				nameAt = pos + 40
			}
			if nameAt+nameSize > pos+hsize {
				return Header{}, errTruncated
			}
			if flags&0x00e0 == 0x00e0 { // directory entry
				pos += hsize + int(pack)
				continue
			}
			if flags&0x0004 != 0 {
				return Header{}, ErrEncrypted
			}
			if method != 0x30 {
				return Header{}, ErrCompressed
			}
			name := buf[nameAt : nameAt+nameSize]
			if i := bytes.IndexByte(name, 0); i >= 0 { // ASCII name, then encoded Unicode
				name = name[:i]
			}
			return Header{
				Format:       4,
				InnerName:    cleanName(string(name)),
				DataOffset:   int64(pos + hsize),
				PackSize:     pack,
				UnpackedSize: unp,
			}, nil
		case 0x7b: // end of archive
			return Header{}, ErrNoFile
		default: // marker, comment, sub-blocks: skip, with their data
			skip := int64(hsize)
			if flags&0x8000 != 0 {
				if pos+11 > len(buf) {
					return Header{}, errTruncated
				}
				skip += le32(pos + 7)
			}
			pos += int(skip)
		}
	}
}

// parse5 walks RAR5 headers: CRC32, vint header size, then type, flags,
// optional extra-area and data sizes, type fields and the extra area.
func parse5(buf []byte) (Header, error) {
	pos := len(marker5)
	var mainSize uint64
	for {
		if pos+4 > len(buf) {
			return Header{}, errTruncated
		}
		hs, n := binary.Uvarint(buf[pos+4:])
		if n <= 0 {
			return Header{}, errTruncated
		}
		start := pos + 4 + n
		end := start + int(hs)
		if hs > uint64(len(buf)) || end > len(buf) {
			return Header{}, errTruncated
		}
		h := vintReader{b: buf[start:end]}
		typ, flags := h.next(), h.next()
		var extraSize, dataSize uint64
		if flags&0x0001 != 0 {
			extraSize = h.next()
		}
		if flags&0x0002 != 0 {
			dataSize = h.next()
		}
		switch typ {
		case 1: // archive header
			mainSize = hs
		case 4: // archive encryption header
			return Header{}, ErrEncrypted
		case 2: // file header
			fileFlags := h.next()
			unp := h.next()
			h.next() // attributes
			if fileFlags&0x0002 != 0 {
				h.skip(4) // mtime
			}
			if fileFlags&0x0004 != 0 {
				h.skip(4) // data CRC32
			}
			compInfo := h.next()
			h.next() // host OS
			name := h.bytes(int(h.next()))
			if h.err {
				return Header{}, errTruncated
			}
			if fileFlags&0x0001 != 0 { // directory
				break
			}
			if extraSize > 0 && hasRecord(buf[end-int(extraSize):end], 1) { // file encryption record
				return Header{}, ErrEncrypted
			}
			if (compInfo>>7)&7 != 0 {
				return Header{}, ErrCompressed
			}
			if fileFlags&0x0008 != 0 { // unpacked size unknown
				unp = 0
			}
			return Header{
				Format:         5,
				InnerName:      cleanName(string(name)),
				DataOffset:     int64(end),
				PackSize:       int64(dataSize),
				UnpackedSize:   int64(unp),
				mainHeaderSize: mainSize,
			}, nil
		case 5: // end of archive
			return Header{}, ErrNoFile
		}
		if h.err {
			return Header{}, errTruncated
		}
		pos = end + int(dataSize)
	}
}

// hasRecord reports whether a RAR5 extra area holds a record of type typ.
func hasRecord(extra []byte, typ uint64) bool {
	r := vintReader{b: extra}
	for len(r.b) > 0 && !r.err {
		size := r.next()
		rec := vintReader{b: r.bytes(int(size))}
		if !r.err && rec.next() == typ {
			return true
		}
	}
	return false
}

// vintReader consumes RAR5 variable-length integers (7 bits per byte, low
// bits first — binary.Uvarint's encoding). Overruns set err and yield 0.
type vintReader struct {
	b   []byte
	err bool
}

func (r *vintReader) next() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err, r.b = true, nil
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *vintReader) bytes(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.err, r.b = true, nil
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *vintReader) skip(n int) { r.bytes(n) }

// uvarintLen is the encoded length of v as a RAR5 vint.
func uvarintLen(v uint64) int {
	return len(binary.AppendUvarint(nil, v))
}

// cleanName normalizes an archived path (RAR4 uses backslashes).
func cleanName(name string) string {
	return path.Clean(strings.ReplaceAll(name, `\`, "/"))
}

// Package rartest builds stored multi-volume RAR sets byte by byte for
// tests, so no rar binary is needed. Header and data checksums are real,
// so unrar accepts the generated sets.
package rartest

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Options tweak the generated archive.
type Options struct {
	Format    int  // 4 or 5
	Compress  bool // mark the file as compressed instead of stored
	Encrypt   bool // mark the file as encrypted
	NewNaming bool // x.part01.rar instead of x.rar, x.r00…
}

// Volume is one generated volume.
type Volume struct {
	Name string
	Data []byte
}

// Set splits data across volumes of at most chunk data bytes each and
// returns them in volume order, named after stem.
func Set(stem, inner string, data []byte, chunk int, o Options) []Volume {
	var parts [][]byte
	for len(data) > chunk {
		parts, data = append(parts, data[:chunk]), data[chunk:]
	}
	parts = append(parts, data)
	var whole []byte
	for _, p := range parts {
		whole = append(whole, p...)
	}
	var vols []Volume
	for i, p := range parts {
		last := i == len(parts)-1
		var b []byte
		if o.Format == 5 {
			b = volume5(inner, p, whole, i, last, o)
		} else {
			b = volume4(inner, p, whole, i, last, o)
		}
		vols = append(vols, Volume{Name: name(stem, i, len(parts), o), Data: b})
	}
	return vols
}

func name(stem string, i, n int, o Options) string {
	if o.NewNaming {
		return fmt.Sprintf("%s.part%0*d.rar", stem, len(fmt.Sprint(n)), i+1)
	}
	if i == 0 {
		return stem + ".rar"
	}
	return fmt.Sprintf("%s.%c%02d", stem, 'r'+(i-1)/100, (i-1)%100)
}

func le16(b []byte, v int) []byte { return binary.LittleEndian.AppendUint16(b, uint16(v)) }
func le32(b []byte, v int) []byte { return binary.LittleEndian.AppendUint32(b, uint32(v)) }

// block4 appends a RAR4 block: CRC16 (low half of the CRC32 of the rest
// of the header), type, flags, size, body.
func block4(b []byte, typ byte, flags int, body []byte) []byte {
	h := append(le16(le16([]byte{typ}, flags), 7+len(body)), body...)
	return append(le16(b, int(crc32.ChecksumIEEE(h)&0xffff)), h...)
}

func volume4(inner string, data, whole []byte, i int, last bool, o Options) []byte {
	b := []byte("Rar!\x1a\x07\x00")
	// archive header: volume flag, first-volume flag on volume 0
	mainFlags := 0x0001
	if i == 0 {
		mainFlags |= 0x0100
	}
	if o.NewNaming {
		mainFlags |= 0x0010
	}
	b = block4(b, 0x73, mainFlags, make([]byte, 6))

	fileFlags := 0x8000
	if i > 0 {
		fileFlags |= 0x0001 // split before
	}
	if !last {
		fileFlags |= 0x0002 // split after
	}
	if o.Encrypt {
		fileFlags |= 0x0004
	}
	method := byte(0x30)
	if o.Compress {
		method = 0x33
	}
	// Non-final parts carry the CRC of their own data, the final part
	// the CRC of the whole file.
	crc := crc32.ChecksumIEEE(data)
	if last {
		crc = crc32.ChecksumIEEE(whole)
	}
	f := le32(le32(nil, len(data)), len(whole))
	f = append(f, 2) // host OS: Windows
	f = le32(le32(f, int(crc)), 0x21000000)
	f = append(f, 29, method)
	f = le32(le16(f, len(inner)), 0x20)
	f = append(f, inner...)
	b = append(block4(b, 0x74, fileFlags, f), data...)

	// end of archive with data CRC and volume number (constant size
	// across volumes)
	endFlags := 0x4000 | 0x0002 | 0x0008
	if !last {
		endFlags |= 0x0001
	}
	return block4(b, 0x7b, endFlags, le16(le32(nil, int(crc32.ChecksumIEEE(data))), i))
}

func vint(b []byte, v int) []byte { return binary.AppendUvarint(b, uint64(v)) }

// block5 appends a RAR5 header: CRC32 of everything after it, the vint
// header size, then the header itself.
func block5(b, h []byte) []byte {
	sized := append(vint(nil, len(h)), h...)
	return append(le32(b, int(crc32.ChecksumIEEE(sized))), sized...)
}

func volume5(inner string, data, whole []byte, i int, last bool, o Options) []byte {
	b := []byte("Rar!\x1a\x07\x01\x00")

	main := vint(vint(nil, 1), 0)
	if i == 0 {
		main = vint(main, 0x0001)
	} else {
		main = vint(vint(main, 0x0001|0x0002), i)
	}
	b = block5(b, main)

	hflags := 0x0002
	if i > 0 {
		hflags |= 0x0008
	}
	if !last {
		hflags |= 0x0010
	}
	fileFlags := 0
	if last {
		fileFlags |= 0x0004 // data CRC only in the final part
	}
	var extra []byte
	if o.Encrypt {
		rec := vint(nil, 1)
		rec = append(rec, make([]byte, 4)...)
		extra = append(vint(nil, len(rec)), rec...)
		hflags |= 0x0001
	}
	method := 0
	if o.Compress {
		method = 3
	}
	f := vint(nil, 2)
	f = vint(f, hflags)
	if len(extra) > 0 {
		f = vint(f, len(extra))
	}
	f = vint(f, len(data))
	f = vint(vint(vint(f, fileFlags), len(whole)), 0x20)
	if fileFlags&0x0004 != 0 {
		f = le32(f, int(crc32.ChecksumIEEE(whole)))
	}
	f = vint(vint(vint(f, method<<7), 0), len(inner))
	f = append(append(f, inner...), extra...)
	b = append(block5(b, f), data...)

	endFlags := 0
	if !last {
		endFlags = 1
	}
	return block5(b, vint(vint(vint(nil, 5), 0), endFlags))
}

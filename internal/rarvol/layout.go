package rarvol

import (
	"errors"
	"fmt"
)

// ErrLayout is returned when a set's volumes don't add up to its file.
var ErrLayout = errors.New("RAR volume layout mismatch")

// Segment is one volume's slice of the virtual file.
type Segment struct {
	FileIndex  int   // torrent file index of the volume
	DataOffset int64 // start of the data block inside the volume
	Length     int64
	// Verified is false while the segment is predicted rather than read
	// from its volume's header; Resolve must pass before its bytes are
	// served.
	Verified bool
}

// Layout maps the virtual (inner) file onto the set's volumes.
type Layout struct {
	InnerName string
	Size      int64
	Segments  []Segment
}

// Build lays the inner file out over set from the first volume's header
// alone, so playback can start before any other volume has arrived.
// Scene volumes are cut to one size and repeat the first volume's
// headers and trailer, so each middle volume's data block is predicted
// from those — plus, in RAR5, the volume-number field the archive header
// gains from the second volume on. The last volume carries the
// remainder; its headers may differ (RAR5 puts the file checksum in the
// final part), so its data offset is only settled by Resolve.
func Build(set Set, h0 Header) (Layout, error) {
	l := Layout{InnerName: h0.InnerName}
	n := len(set.Files)
	if n == 1 {
		l.Size = h0.PackSize
		l.Segments = []Segment{{FileIndex: set.First().Index, DataOffset: h0.DataOffset, Length: h0.PackSize, Verified: true}}
		return l, nil
	}
	if h0.UnpackedSize <= 0 {
		return Layout{}, fmt.Errorf("%w: unpacked size unknown", ErrLayout)
	}
	trailer := set.Files[0].Size - h0.DataOffset - h0.PackSize
	var sum int64
	for i, f := range set.Files {
		seg := Segment{FileIndex: f.Index, DataOffset: h0.DataOffset, Length: h0.PackSize, Verified: i == 0}
		if i > 0 {
			seg.DataOffset += headerGrowth(h0, i)
			seg.Length = f.Size - seg.DataOffset - trailer
		}
		if i == n-1 {
			seg.Length = h0.UnpackedSize - sum
		}
		if seg.Length <= 0 || seg.DataOffset+seg.Length > f.Size {
			return Layout{}, fmt.Errorf("%w: volume %d data [%d,+%d) outside its %d bytes",
				ErrLayout, i, seg.DataOffset, seg.Length, f.Size)
		}
		l.Segments = append(l.Segments, seg)
		sum += seg.Length
	}
	l.Size = sum
	return l, nil
}

// headerGrowth is how many bytes longer volume i's headers are than the
// first volume's: a RAR5 archive header stores the volume number from
// the second volume on, which can also lengthen its own size vint.
func headerGrowth(h0 Header, i int) int64 {
	if h0.Format != 5 || h0.mainHeaderSize == 0 {
		return 0
	}
	field := uint64(uvarintLen(uint64(i)))
	return int64(field) + int64(uvarintLen(h0.mainHeaderSize+field)-uvarintLen(h0.mainHeaderSize))
}

// Resolve checks segment i against its volume's real header. A middle
// volume must match its prediction exactly; the last volume adopts its
// header's data offset, as long as it carries exactly the remainder.
func (l *Layout) Resolve(i int, h Header, volumeSize int64) error {
	seg := &l.Segments[i]
	if seg.Verified {
		return nil
	}
	if h.InnerName != l.InnerName || h.PackSize != seg.Length {
		return fmt.Errorf("%w: volume %d holds %q with %d bytes, want %q with %d",
			ErrLayout, i, h.InnerName, h.PackSize, l.InnerName, seg.Length)
	}
	if i == len(l.Segments)-1 {
		if h.DataOffset+h.PackSize > volumeSize {
			return fmt.Errorf("%w: last volume data [%d,+%d) outside its %d bytes", ErrLayout, h.DataOffset, h.PackSize, volumeSize)
		}
		seg.DataOffset = h.DataOffset
	} else if h.DataOffset != seg.DataOffset {
		return fmt.Errorf("%w: volume %d data starts at %d, predicted %d", ErrLayout, i, h.DataOffset, seg.DataOffset)
	}
	seg.Verified = true
	return nil
}

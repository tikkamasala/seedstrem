package stream

import (
	"errors"
	"fmt"
	"io"

	"github.com/kipsilabs/seedstrem/internal/rarvol"
)

// rarReader is an io.ReadSeeker over the file inside a stored RAR set.
// Each read is served by a partialReader over one volume's data block,
// so piece waits, hints and stall detection work exactly as for a plain
// file; a read never crosses a volume boundary.
type rarReader struct {
	layout *rarvol.Layout
	offset int64

	// open returns a reader over segment i's data block, checking a
	// predicted segment against its volume's header first.
	open func(i int) (*partialReader, error)
	// enter / leave report the volume this reader is in, for the
	// download window.
	enter func(i int)
	leave func(i int)

	cur       int
	curR      *partialReader
	delivered int64
}

func (rr *rarReader) Read(p []byte) (int, error) {
	if rr.offset >= rr.layout.Size {
		return 0, io.EOF
	}
	i, segOff := rr.locate(rr.offset)
	if rr.curR == nil || i != rr.cur {
		// Enter the next volume before leaving the current one: leaving
		// first would, for a moment, unwant a volume still downloading.
		rr.enter(i)
		r, err := rr.open(i)
		rr.closeCur()
		if err != nil {
			rr.leave(i)
			return 0, fmt.Errorf("rar volume %d: %w", i, err)
		}
		rr.cur, rr.curR = i, r
	}
	if remaining := rr.layout.Segments[i].Length - segOff; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	rr.curR.offset = segOff
	n, err := rr.curR.Read(p)
	rr.offset += int64(n)
	rr.delivered += int64(n)
	if errors.Is(err, io.EOF) {
		err = nil // end of this volume's block; the next read moves on
	}
	return n, err
}

// locate maps a virtual offset to its segment and the offset inside it.
func (rr *rarReader) locate(off int64) (int, int64) {
	for i, seg := range rr.layout.Segments {
		if off < seg.Length {
			return i, off
		}
		off -= seg.Length
	}
	last := len(rr.layout.Segments) - 1
	return last, rr.layout.Segments[last].Length
}

func (rr *rarReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = rr.offset + offset
	case io.SeekEnd:
		target = rr.layout.Size + offset
	default:
		return 0, fmt.Errorf("bad whence %d", whence)
	}
	if target < 0 {
		return 0, errors.New("negative seek position")
	}
	rr.offset = target
	return target, nil
}

func (rr *rarReader) closeCur() {
	if rr.curR == nil {
		return
	}
	rr.curR.Close()
	rr.leave(rr.cur)
	rr.curR = nil
}

func (rr *rarReader) Close() error {
	rr.closeCur()
	return nil
}

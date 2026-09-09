package tray

import "encoding/binary"

// pickIconEntry parses an ICO container and returns the byte range of the image
// best suited to a want-pixel target: the smallest image at least that large,
// or — when every image is smaller — the largest available. Downscaling stays
// sharp while upscaling does not, so an exact match is preferred, then a bigger
// image, and only last a smaller one.
// It is pure parsing (no syscalls) so it can be tested on any platform.
func pickIconEntry(data []byte, want int) (offset, size uint32, ok bool) {
	const (
		dirSize   = 6
		entrySize = 16
	)
	if len(data) < dirSize {
		return 0, 0, false
	}
	if binary.LittleEndian.Uint16(data[0:2]) != 0 || binary.LittleEndian.Uint16(data[2:4]) != 1 {
		return 0, 0, false // reserved must be 0 and type must be 1 (icon)
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count == 0 || len(data) < dirSize+count*entrySize {
		return 0, 0, false
	}

	bestUp, bestUpW := -1, 1<<30 // smallest image >= want
	bestDown, bestDownW := -1, 0 // largest image < want
	for i := 0; i < count; i++ {
		e := data[dirSize+i*entrySize : dirSize+(i+1)*entrySize]
		w := int(e[0])
		if w == 0 {
			w = 256 // 0 means 256 in the ICO format
		}
		if w >= want {
			if w < bestUpW {
				bestUpW, bestUp = w, i
			}
		} else if w > bestDownW {
			bestDownW, bestDown = w, i
		}
	}

	best := bestUp
	if best < 0 {
		best = bestDown
	}
	if best < 0 {
		return 0, 0, false
	}

	e := data[dirSize+best*entrySize : dirSize+(best+1)*entrySize]
	size = binary.LittleEndian.Uint32(e[8:12])
	offset = binary.LittleEndian.Uint32(e[12:16])
	if size == 0 || uint64(offset)+uint64(size) > uint64(len(data)) {
		return 0, 0, false
	}
	return offset, size, true
}

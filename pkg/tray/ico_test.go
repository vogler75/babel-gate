package tray

import (
	"encoding/binary"
	"os"
	"testing"
)

// icoBytes is the real embedded tray icon, read from disk so this test runs on
// every platform (the go:embed of the same file is Windows-only).
func icoBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("icon.ico")
	if err != nil {
		t.Fatalf("reading icon.ico: %v", err)
	}
	return data
}

func TestPickIconEntryRealIcon(t *testing.T) {
	data := icoBytes(t)

	// The shipped icon must expose the sizes Windows asks for.
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count < 2 {
		t.Fatalf("expected a multi-resolution icon, got %d image(s)", count)
	}

	for _, want := range []int{16, 20, 24, 32, 48} {
		offset, size, ok := pickIconEntry(data, want)
		if !ok {
			t.Fatalf("pickIconEntry(%d) failed", want)
		}
		if uint64(offset)+uint64(size) > uint64(len(data)) {
			t.Fatalf("pickIconEntry(%d) returned range %d..%d outside the %d byte file",
				want, offset, offset+size, len(data))
		}
		// Every image is an uncompressed DIB: it must start with a 40-byte
		// BITMAPINFOHEADER whose height is twice the width (XOR + AND masks).
		hdr := data[offset : offset+size]
		if got := binary.LittleEndian.Uint32(hdr[0:4]); got != 40 {
			t.Errorf("size %d: biSize = %d, want 40", want, got)
		}
		w := int32(binary.LittleEndian.Uint32(hdr[4:8]))
		h := int32(binary.LittleEndian.Uint32(hdr[8:12]))
		if h != w*2 {
			t.Errorf("size %d: biHeight = %d, want 2*biWidth (%d)", want, h, w*2)
		}
		if got := binary.LittleEndian.Uint16(hdr[14:16]); got != 32 {
			t.Errorf("size %d: biBitCount = %d, want 32", want, got)
		}
	}
}

func TestPickIconEntryExactMatch(t *testing.T) {
	data := icoBytes(t)
	count := int(binary.LittleEndian.Uint16(data[4:6]))

	for i := 0; i < count; i++ {
		e := data[6+i*16 : 6+(i+1)*16]
		width := int(e[0])
		if width == 0 {
			continue // 256px entry
		}
		offset, _, ok := pickIconEntry(data, width)
		if !ok {
			t.Fatalf("pickIconEntry(%d) failed", width)
		}
		wantOffset := binary.LittleEndian.Uint32(e[12:16])
		if offset != wantOffset {
			t.Errorf("pickIconEntry(%d) chose offset %d, want the exact-size entry at %d",
				width, offset, wantOffset)
		}
	}
}

func TestPickIconEntryPrefersDownscale(t *testing.T) {
	// Two entries, 16px and 48px; asking for 24 should pick 48 (downscale)
	// rather than 16, because upscaling is penalised.
	data := buildICO(t, []int{16, 48})
	offset, _, ok := pickIconEntry(data, 24)
	if !ok {
		t.Fatal("pickIconEntry failed")
	}
	e := data[6+16 : 6+32] // second entry (48px)
	if want := binary.LittleEndian.Uint32(e[12:16]); offset != want {
		t.Errorf("chose offset %d, want the 48px entry at %d", offset, want)
	}
}

func TestPickIconEntryFallsBackToLargest(t *testing.T) {
	// Nothing is big enough for a 64px request, so the largest (32) must win.
	data := buildICO(t, []int{16, 32})
	offset, _, ok := pickIconEntry(data, 64)
	if !ok {
		t.Fatal("pickIconEntry failed")
	}
	e := data[6+16 : 6+32] // second entry (32px)
	if want := binary.LittleEndian.Uint32(e[12:16]); offset != want {
		t.Errorf("chose offset %d, want the 32px entry at %d", offset, want)
	}
}

func TestPickIconEntryTreatsZeroWidthAs256(t *testing.T) {
	data := buildICO(t, []int{16, 256}) // 256 is encoded as a width byte of 0
	if got := data[6+16]; got != 0 {
		t.Fatalf("test fixture: expected 256px entry to encode width 0, got %d", got)
	}
	// A 32px request has no exact match; the 256px entry is the only one at
	// least that large, so it must be chosen over the 16px one.
	offset, _, ok := pickIconEntry(data, 32)
	if !ok {
		t.Fatal("pickIconEntry failed")
	}
	e := data[6+16 : 6+32]
	if want := binary.LittleEndian.Uint32(e[12:16]); offset != want {
		t.Errorf("chose offset %d, want the 256px entry at %d", offset, want)
	}
}

func TestPickIconEntryRejectsMalformed(t *testing.T) {
	good := icoBytes(t)

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"truncated header", good[:4]},
		{"wrong type", func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint16(b[2:4], 2) // 2 = cursor, not icon
			return b
		}()},
		{"reserved not zero", func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint16(b[0:2], 1)
			return b
		}()},
		{"zero images", func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint16(b[4:6], 0)
			return b
		}()},
		{"entry table truncated", good[:20]},
		{"image range past EOF", func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint32(b[6+12:6+16], uint32(len(b))) // offset at EOF
			binary.LittleEndian.PutUint32(b[6+8:6+12], 1024)            // but claims 1KB
			binary.LittleEndian.PutUint16(b[4:6], 1)                    // single entry
			return b
		}()},
		{"zero-length image", func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint16(b[4:6], 1)
			binary.LittleEndian.PutUint32(b[6+8:6+12], 0)
			return b
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := pickIconEntry(tc.data, 16); ok {
				t.Error("expected pickIconEntry to reject malformed input")
			}
		})
	}
}

// buildICO writes a minimal well-formed ICO with dummy image payloads.
func buildICO(t *testing.T, sizes []int) []byte {
	t.Helper()
	const payload = 64
	out := make([]byte, 6+16*len(sizes))
	binary.LittleEndian.PutUint16(out[2:4], 1)
	binary.LittleEndian.PutUint16(out[4:6], uint16(len(sizes)))

	offset := uint32(len(out))
	for i, s := range sizes {
		e := out[6+i*16 : 6+(i+1)*16]
		e[0], e[1] = byte(s), byte(s)
		binary.LittleEndian.PutUint16(e[4:6], 1)
		binary.LittleEndian.PutUint16(e[6:8], 32)
		binary.LittleEndian.PutUint32(e[8:12], payload)
		binary.LittleEndian.PutUint32(e[12:16], offset)
		offset += payload
	}
	return append(out, make([]byte, payload*len(sizes))...)
}

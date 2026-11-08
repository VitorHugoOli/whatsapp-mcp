package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func solidImage(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation inserts an APP1 EXIF segment with the given Orientation
// right after the JPEG SOI marker.
func withOrientation(jpg []byte, orientation uint16) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00") // little endian, IFD0 at offset 8
	entry := make([]byte, 2+12+4)
	binary.LittleEndian.PutUint16(entry[0:], 1)      // one entry
	binary.LittleEndian.PutUint16(entry[2:], 0x0112) // Orientation
	binary.LittleEndian.PutUint16(entry[4:], 3)      // SHORT
	binary.LittleEndian.PutUint32(entry[6:], 1)      // count
	binary.LittleEndian.PutUint16(entry[10:], orientation)
	payload := append([]byte("Exif\x00\x00"), append(tiff, entry...)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

func decodeThumb(t *testing.T, thumb []byte) image.Image {
	t.Helper()
	if len(thumb) == 0 {
		t.Fatal("thumbnail is empty")
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("thumbnail is not a valid JPEG: %v", err)
	}
	return img
}

func TestImageMetadataWideJPEG(t *testing.T) {
	data := encodeJPEG(t, solidImage(1237, 493, color.RGBA{200, 30, 30, 255}))
	meta, err := imageMetadata(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Width != 1237 || meta.Height != 493 {
		t.Fatalf("got %dx%d, want 1237x493", meta.Width, meta.Height)
	}
	thumb := decodeThumb(t, meta.Thumbnail)
	if b := thumb.Bounds(); b.Dx() != 100 || b.Dy() != 39 {
		t.Fatalf("thumbnail is %dx%d, want 100x39 (same aspect ratio)", b.Dx(), b.Dy())
	}
	if len(meta.Thumbnail) > 10_000 {
		t.Fatalf("thumbnail too large: %d bytes", len(meta.Thumbnail))
	}
	// The colour survives the downscale.
	r, g, b, _ := thumb.At(50, 20).RGBA()
	if r>>8 < 170 || g>>8 > 70 || b>>8 > 70 {
		t.Fatalf("thumbnail colour off: %d,%d,%d", r>>8, g>>8, b>>8)
	}
}

func TestImageMetadataTallAndSmallImages(t *testing.T) {
	meta, err := imageMetadata(encodeJPEG(t, solidImage(300, 900, color.White)))
	if err != nil || meta.Width != 300 || meta.Height != 900 {
		t.Fatalf("tall: %+v, %v", meta, err)
	}
	if b := decodeThumb(t, meta.Thumbnail).Bounds(); b.Dx() != 33 || b.Dy() != 100 {
		t.Fatalf("tall thumbnail %dx%d, want 33x100", b.Dx(), b.Dy())
	}
	// Never upscaled.
	meta, err = imageMetadata(encodeJPEG(t, solidImage(40, 20, color.White)))
	if err != nil {
		t.Fatal(err)
	}
	if b := decodeThumb(t, meta.Thumbnail).Bounds(); b.Dx() != 40 || b.Dy() != 20 {
		t.Fatalf("small thumbnail %dx%d, want 40x20", b.Dx(), b.Dy())
	}
}

func TestImageMetadataPNGWithTransparency(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 640, 480)) // fully transparent
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	meta, err := imageMetadata(buf.Bytes())
	if err != nil || meta.Width != 640 || meta.Height != 480 {
		t.Fatalf("png: %+v, %v", meta, err)
	}
	// Transparency becomes white, not black.
	r, g, b, _ := decodeThumb(t, meta.Thumbnail).At(10, 10).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent pixels should be white, got %d,%d,%d", r>>8, g>>8, b>>8)
	}
}

func TestImageMetadataEXIFOrientation(t *testing.T) {
	base := encodeJPEG(t, solidImage(400, 100, color.White)) // stored landscape
	for orientation, want := range map[uint16][2]uint32{
		1: {400, 100}, 3: {400, 100}, 6: {100, 400}, 8: {100, 400},
	} {
		data := withOrientation(base, orientation)
		if got := jpegOrientation(data); got != int(orientation) {
			t.Fatalf("jpegOrientation = %d, want %d", got, orientation)
		}
		meta, err := imageMetadata(data)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Width != want[0] || meta.Height != want[1] {
			t.Fatalf("orientation %d: got %dx%d, want %dx%d", orientation, meta.Width, meta.Height, want[0], want[1])
		}
		tb := decodeThumb(t, meta.Thumbnail).Bounds()
		if (tb.Dx() > tb.Dy()) != (want[0] > want[1]) {
			t.Fatalf("orientation %d: thumbnail %dx%d not upright", orientation, tb.Dx(), tb.Dy())
		}
	}
}

func TestImageMetadataInvalidData(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"text":      []byte("not an image at all"),
		"truncated": encodeJPEG(t, solidImage(50, 50, color.White))[:20],
		"bad exif":  withOrientation(encodeJPEG(t, solidImage(10, 10, color.White)), 99),
	} {
		meta, err := imageMetadata(data)
		if name == "bad exif" {
			// Unreadable orientation falls back to "as stored", not an error.
			if err != nil || meta.Width != 10 || meta.Height != 10 {
				t.Fatalf("%s: %+v, %v", name, meta, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if meta.Width != 0 || meta.Height != 0 || meta.Thumbnail != nil {
			t.Fatalf("%s: expected empty metadata, got %+v", name, meta)
		}
	}
	// Malformed EXIF must never panic.
	for _, data := range [][]byte{
		{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0},
		{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF},
		{0xFF, 0xD8},
	} {
		if got := jpegOrientation(data); got != 1 {
			t.Fatalf("malformed EXIF gave orientation %d", got)
		}
	}
}

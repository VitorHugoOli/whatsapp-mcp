package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // registers GIF for image.Decode / DecodeConfig
	"image/jpeg"
	_ "image/png" // registers PNG for image.Decode / DecodeConfig
)

// Outgoing image previews. Without Width/Height on the ImageMessage, WhatsApp
// clients (the iPhone app in particular) cannot tell the aspect ratio and
// show the image cropped to a square; JPEGThumbnail is the blurred preview
// they draw before the download finishes.
//
// Only the standard library is used, so JPEG, PNG and GIF are understood.
// WebP (golang.org/x/image) is not: such images are sent as before, without
// dimensions.

const (
	// thumbnailMaxSide is the longer side of JPEGThumbnail, in pixels.
	thumbnailMaxSide = 100
	thumbnailQuality = 60
	// maxThumbnailPixels guards the full decode needed for the thumbnail
	// against decompression bombs; dimensions are still reported above it.
	maxThumbnailPixels = 50_000_000
	// thumbnailSamples is the samples per axis averaged into each thumbnail
	// pixel: enough to look smooth at 100 px, and the cost does not grow with
	// the source size.
	thumbnailSamples = 4
)

type imageMeta struct {
	Width     uint32
	Height    uint32
	Thumbnail []byte
}

// imageMetadata reads the displayed size of an image (EXIF orientation
// applied for JPEG) and builds a small JPEG thumbnail. It returns an error
// only when the size cannot be read; a thumbnail that cannot be built is
// left nil. Callers must treat any error as "send without metadata".
func imageMetadata(data []byte) (imageMeta, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return imageMeta{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return imageMeta{}, errors.New("image has no size")
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}
	meta := imageMeta{Width: uint32(cfg.Width), Height: uint32(cfg.Height)}
	if orientation >= 5 && orientation <= 8 { // rotated 90° either way
		meta.Width, meta.Height = meta.Height, meta.Width
	}

	if int64(cfg.Width)*int64(cfg.Height) <= maxThumbnailPixels {
		if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			meta.Thumbnail = jpegThumbnail(img, orientation)
		}
	}
	return meta, nil
}

// jpegThumbnail scales img so its longer side is thumbnailMaxSide (never
// up), applies the EXIF orientation, flattens transparency onto white and
// encodes it as JPEG. Returns nil if encoding fails.
func jpegThumbnail(img image.Image, orientation int) []byte {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil
	}
	tw, th := sw, sh
	if sw >= sh && sw > thumbnailMaxSide {
		tw, th = thumbnailMaxSide, max(1, sh*thumbnailMaxSide/sw)
	} else if sh > sw && sh > thumbnailMaxSide {
		tw, th = max(1, sw*thumbnailMaxSide/sh), thumbnailMaxSide
	}

	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			small.Set(x, y, averageBlock(img, b, x, y, tw, th))
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, orient(small, orientation), &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// averageBlock averages a fixed grid of samples from the source block that
// maps to thumbnail pixel (x, y), compositing over white.
func averageBlock(img image.Image, b image.Rectangle, x, y, tw, th int) color.Color {
	sw, sh := b.Dx(), b.Dy()
	x0, x1 := x*sw/tw, max(x*sw/tw+1, (x+1)*sw/tw)
	y0, y1 := y*sh/th, max(y*sh/th+1, (y+1)*sh/th)
	var r, g, bl, n uint64
	for sy := 0; sy < thumbnailSamples; sy++ {
		py := y0 + (y1-y0)*(2*sy+1)/(2*thumbnailSamples)
		for sx := 0; sx < thumbnailSamples; sx++ {
			px := x0 + (x1-x0)*(2*sx+1)/(2*thumbnailSamples)
			cr, cg, cb, ca := img.At(b.Min.X+px, b.Min.Y+py).RGBA()
			// Premultiplied: add white for the transparent part.
			white := 0xffff - uint64(ca)
			r += uint64(cr) + white
			g += uint64(cg) + white
			bl += uint64(cb) + white
			n++
		}
	}
	return color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: 0xffff}
}

// orient applies an EXIF orientation (2–8) so the thumbnail is upright.
func orient(src *image.RGBA, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch orientation {
			case 2: // mirrored horizontally
				dx, dy = w-1-x, y
			case 3: // rotated 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirrored vertically
				dx, dy = x, h-1-y
			case 5: // transposed
				dx, dy = y, x
			case 6: // rotated 90° clockwise
				dx, dy = h-1-y, x
			case 7: // transversed
				dx, dy = h-1-y, w-1-x
			case 8: // rotated 90° counter-clockwise
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(x, y))
		}
	}
	return dst
}

// jpegOrientation returns the EXIF Orientation (1–8) of a JPEG, or 1 when it
// has none or it cannot be read. It walks the markers up to the first scan.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			i += 2 // markers without a length
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // start of scan / end of image
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return 1
		}
		segment := data[i+4 : i+2+size]
		if marker == 0xE1 && len(segment) >= 6 && string(segment[:6]) == "Exif\x00\x00" {
			return exifOrientation(segment[6:])
		}
		i += 2 + size
	}
	return 1
}

// exifOrientation reads tag 0x0112 from IFD0 of a TIFF-structured EXIF block.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	for e := 0; e < count; e++ {
		entry := ifd + 2 + e*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) == 0x0112 {
			v := int(order.Uint16(tiff[entry+8 : entry+10]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

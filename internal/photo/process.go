// Package photo validates, compresses and stores visit photos (M4).
package photo

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders for image.DecodeConfig
	_ "image/png"
	"io"
	"net/http"

	"github.com/disintegration/imaging"
)

const (
	// MaxUploadBytes caps one uploaded photo before processing.
	MaxUploadBytes = 10 << 20
	// maxPixels rejects decompression bombs (a tiny file that decodes to a huge bitmap).
	maxPixels = 40_000_000
	// maxSide is the longest edge of the stored image.
	maxSide     = 1280
	jpegQuality = 80
)

var (
	ErrTooLarge     = errors.New("photo too large")
	ErrInvalidImage = errors.New("not a valid JPEG or PNG image")
)

// Process validates an uploaded image and returns it as a compressed JPEG.
//
// Only real JPEG and PNG data is accepted (checked by content, not by the
// client-supplied name or type). The result is rotated according to the EXIF
// orientation, shrunk to at most 1280px on its longest side and re-encoded, which
// also drops all metadata (EXIF, GPS position, camera serial number).
func Process(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read photo: %w", err)
	}
	if len(raw) > MaxUploadBytes {
		return nil, ErrTooLarge
	}
	switch http.DetectContentType(raw) {
	case "image/jpeg", "image/png":
	default:
		return nil, ErrInvalidImage
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, ErrInvalidImage
	}
	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, ErrInvalidImage
	}
	if b := img.Bounds(); b.Dx() > maxSide || b.Dy() > maxSide {
		img = imaging.Fit(img, maxSide, maxSide, imaging.Lanczos)
	}
	var out bytes.Buffer
	if err := imaging.Encode(&out, img, imaging.JPEG, imaging.JPEGQuality(jpegQuality)); err != nil {
		return nil, fmt.Errorf("encode photo: %w", err)
	}
	return out.Bytes(), nil
}

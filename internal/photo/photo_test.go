package photo

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"
)

// testJPEG returns a w x h gradient JPEG, optionally with an EXIF APP1 segment right after SOI.
func testJPEG(t *testing.T, w, h int, exif []byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if exif == nil {
		return b
	}
	seg := append([]byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}, exif...)
	return append(append(append([]byte{}, b[:2]...), seg...), b[2:]...)
}

// exifOrientation builds a minimal EXIF payload carrying an orientation tag plus a fake GPS string.
func exifOrientation(o byte) []byte {
	tiff := []byte{
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, // little-endian header, IFD at offset 8
		0x01, 0x00, // one entry
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, o, 0x00, 0x00, 0x00, // Orientation = o
		0x00, 0x00, 0x00, 0x00, // no next IFD
	}
	return append(append([]byte("Exif\x00\x00"), tiff...), []byte("GPS-SECRET-1234")...)
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	return img
}

func TestProcessShrinksAndStripsMetadata(t *testing.T) {
	in := testJPEG(t, 3000, 2000, exifOrientation(1))
	if !bytes.Contains(in, []byte("GPS-SECRET")) {
		t.Fatal("test setup: metadata missing from input")
	}
	out, err := Process(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	b := decodeJPEG(t, out).Bounds()
	if b.Dx() != 1280 || b.Dy() != 853 {
		t.Fatalf("size = %dx%d, want 1280x853 (aspect ratio kept)", b.Dx(), b.Dy())
	}
	if len(out) >= len(in) {
		t.Fatalf("not compressed: %d >= %d bytes", len(out), len(in))
	}
	if bytes.Contains(out, []byte("GPS-SECRET")) || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("EXIF metadata survived re-encoding")
	}
}

func TestProcessKeepsSmallImageSize(t *testing.T) {
	out, err := Process(bytes.NewReader(testJPEG(t, 640, 480, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if b := decodeJPEG(t, out).Bounds(); b.Dx() != 640 || b.Dy() != 480 {
		t.Fatalf("small image resized to %dx%d", b.Dx(), b.Dy())
	}
}

func TestProcessAppliesExifOrientation(t *testing.T) {
	// Phones store portrait shots as landscape pixels plus orientation=6 (rotate 90 CW).
	out, err := Process(bytes.NewReader(testJPEG(t, 40, 20, exifOrientation(6))))
	if err != nil {
		t.Fatal(err)
	}
	if b := decodeJPEG(t, out).Bounds(); b.Dx() != 20 || b.Dy() != 40 {
		t.Fatalf("orientation not applied: %dx%d, want 20x40", b.Dx(), b.Dy())
	}
}

func TestProcessAcceptsPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 50, 50))); err != nil {
		t.Fatal(err)
	}
	out, err := Process(&buf)
	if err != nil {
		t.Fatal(err)
	}
	decodeJPEG(t, out) // always stored as JPEG
}

func TestProcessRejects(t *testing.T) {
	var bomb bytes.Buffer // tiny on disk, ~49 megapixels once decoded
	if err := png.Encode(&bomb, image.NewGray(image.Rect(0, 0, 7000, 7000))); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"plain text", []byte("not an image at all"), ErrInvalidImage},
		{"empty", nil, ErrInvalidImage},
		{"jpeg magic with garbage", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(strings.Repeat("x", 200))...), ErrInvalidImage},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), ErrInvalidImage},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ErrInvalidImage},
		{"too large", append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, MaxUploadBytes)...), ErrTooLarge},
		{"pixel bomb", bomb.Bytes(), ErrInvalidImage},
	}
	for _, c := range cases {
		if _, err := Process(bytes.NewReader(c.data)); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestLocalStore(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	key, err := s.Save(ctx, []byte("jpeg-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(key, ".jpg") || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		t.Fatalf("odd key %q", key)
	}
	other, _ := s.Save(ctx, []byte("other"))
	if other == key {
		t.Fatal("keys must be unique")
	}

	f, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "jpeg-bytes" {
		t.Fatalf("read back %q", got)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing photo must not fail: %v", err)
	}
}

func TestLocalStoreCannotEscapeItsDirectory(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	s, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Save(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../" + outside, "../../etc/passwd", "/etc/passwd", `..\..\windows\win.ini`} {
		if f, err := s.Open(context.Background(), key); err == nil {
			f.Close()
			t.Errorf("Open(%q) succeeded", key)
		}
	}
}

package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func picture(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return img
}

func encJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A minimal valid lossless WebP (1x1), so the WebP path runs without an encoder.
var tinyWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x1a, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x4c,
	0x0d, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00,
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	if f, err := Sniff(b); err != nil || f != JPEG {
		t.Fatalf("output must be JPEG, sniffed %q %v", f, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPrepareFormatsAndDownscale(t *testing.T) {
	for name, in := range map[string][]byte{
		"jpeg": encJPEG(t, picture(800, 600)),
		"png":  encPNG(t, picture(800, 600)),
	} {
		out, info, err := Prepare(in, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Width != 800 || info.Height != 600 || string(info.Format) != name {
			t.Errorf("%s: info = %+v", name, info)
		}
		if b := decode(t, out).Bounds(); b.Dy() != TargetShortSide || b.Dx() != 448 {
			t.Errorf("%s: output %v, want short side %d keeping aspect", name, b, TargetShortSide)
		}
	}
	out, info, err := Prepare(tinyWebP, nil)
	if err != nil || info.Format != WebP || decode(t, out).Bounds().Dx() != 1 {
		t.Fatalf("webp: %+v %v", info, err)
	}
}

func TestPrepareCrop(t *testing.T) {
	in := encPNG(t, picture(200, 100))
	out, _, err := Prepare(in, &Crop{X: 50, Y: 10, W: 40, H: 30})
	if err != nil {
		t.Fatal(err)
	}
	if b := decode(t, out).Bounds(); b.Dx() != 40 || b.Dy() != 30 {
		t.Errorf("cropped size = %v", b)
	}
	for _, c := range []Crop{{X: 190, Y: 0, W: 20, H: 10}, {X: -1, Y: 0, W: 5, H: 5}, {X: 0, Y: 0, W: 0, H: 5}} {
		if _, _, err := Prepare(in, &c); !errors.Is(err, ErrCrop) {
			t.Errorf("crop %+v: %v", c, err)
		}
	}
}

func TestPrepareRejects(t *testing.T) {
	jpg := encJPEG(t, picture(16, 16))
	pngBytes := encPNG(t, picture(16, 16))
	huge := make([]byte, MaxBytes+1)
	copy(huge, jpg)

	// A PNG header declaring 5000x5000 pixels: rejected from the header, before decoding.
	wide := bytes.Clone(pngBytes)
	wide[16], wide[17], wide[18], wide[19] = 0, 0, 0x13, 0x88
	wide[20], wide[21], wide[22], wide[23] = 0, 0, 0x13, 0x88
	binary.BigEndian.PutUint32(wide[29:33], crc32.ChecksumIEEE(wide[12:29]))

	cases := map[string]struct {
		in   []byte
		want error
	}{
		"empty":              {nil, ErrUnsupported},
		"gif":                {[]byte("GIF89a\x01\x00\x01\x00"), ErrUnsupported},
		"svg":                {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), ErrUnsupported},
		"too large":          {huge, ErrTooLarge},
		"truncated jpeg":     {jpg[:40], ErrMalformed},
		"jpeg magic, png":    {append([]byte{0xFF, 0xD8, 0xFF}, pngBytes...), ErrMalformed},
		"png magic, jpeg":    {append(bytes.Clone(pngBytes[:8]), jpg...), ErrMalformed},
		"webp magic only":    {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), ErrMalformed},
		"oversized pixels":   {wide, ErrDimensions},
		"html after jpeg":    {append(bytes.Clone(jpg[:2]), []byte("<html><script>")...), ErrUnsupported},
		"zip with jpeg tail": {append([]byte("PK\x03\x04"), jpg...), ErrUnsupported},
	}
	for name, tc := range cases {
		if _, _, err := Prepare(tc.in, nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestPrepareStripsMetadata(t *testing.T) {
	jpg := encJPEG(t, picture(32, 32))
	exif := append([]byte{0xFF, 0xE1, 0x00, 0x10}, []byte("Exif\x00\x00GPS-SECRET")...)
	withExif := append(append(bytes.Clone(jpg[:2]), exif...), jpg[2:]...)
	if _, _, err := Prepare(withExif, nil); err != nil {
		t.Fatal(err)
	}
	out, _, _ := Prepare(withExif, nil)
	if bytes.Contains(out, []byte("GPS-SECRET")) || bytes.Contains(out, []byte("Exif")) {
		t.Error("re-encoding must drop EXIF")
	}
}

package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// Upload limits (docs/ARCHITECTURE.md §9).
const (
	MaxBytes     = 10 << 20
	MaxDimension = 4096
	// TargetShortSide is the short side images are scaled down to before embedding. CLIP resizes
	// to 224 anyway; keeping some headroom costs nothing and makes the payload small.
	TargetShortSide = 336
	JPEGQuality     = 90
)

// Format is a detected image format.
type Format string

// Accepted formats.
const (
	JPEG Format = "jpeg"
	PNG  Format = "png"
	WebP Format = "webp"
)

// Errors map to HTTP statuses: ErrTooLarge 413, ErrUnsupported 415, the rest 400.
var (
	ErrTooLarge    = errors.New("image exceeds 10 MB")
	ErrUnsupported = errors.New("image must be JPEG, PNG or WebP")
	ErrMalformed   = errors.New("image cannot be decoded")
	ErrDimensions  = errors.New("image dimensions are out of range")
	ErrCrop        = errors.New("crop box is outside the image")
)

// Crop is a box in source pixel coordinates.
type Crop struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Info describes an accepted upload.
type Info struct {
	Format Format
	Width  int
	Height int
}

// Sniff detects the format from magic bytes only; the extension and declared content type are
// never trusted.
func Sniff(b []byte) (Format, error) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return JPEG, nil
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return PNG, nil
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return WebP, nil
	}
	return "", ErrUnsupported
}

type codec struct {
	config func(r *bytes.Reader) (image.Config, error)
	decode func(r *bytes.Reader) (image.Image, error)
}

func codecFor(f Format) codec {
	switch f {
	case JPEG:
		return codec{func(r *bytes.Reader) (image.Config, error) { return jpeg.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return jpeg.Decode(r) }}
	case PNG:
		return codec{func(r *bytes.Reader) (image.Config, error) { return png.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return png.Decode(r) }}
	default:
		return codec{func(r *bytes.Reader) (image.Config, error) { return webp.DecodeConfig(r) },
			func(r *bytes.Reader) (image.Image, error) { return webp.Decode(r) }}
	}
}

// Prepare validates an upload and returns it as a fresh JPEG: size cap, magic-byte format,
// dimensions checked from the header before any full decode, optional crop, downscale to
// TargetShortSide, and re-encoding (which drops EXIF and every other metadata block). The
// decoder is chosen by the sniffed format, so a polyglot file is decoded as what its magic
// says or rejected.
func Prepare(b []byte, crop *Crop) ([]byte, Info, error) {
	if len(b) > MaxBytes {
		return nil, Info{}, ErrTooLarge
	}
	f, err := Sniff(b)
	if err != nil {
		return nil, Info{}, err
	}
	c := codecFor(f)
	cfg, err := c.config(bytes.NewReader(b))
	if err != nil {
		return nil, Info{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	info := Info{Format: f, Width: cfg.Width, Height: cfg.Height}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxDimension || cfg.Height > MaxDimension {
		return nil, info, fmt.Errorf("%w: %dx%d (max %dx%d)", ErrDimensions, cfg.Width, cfg.Height, MaxDimension, MaxDimension)
	}
	img, err := c.decode(bytes.NewReader(b))
	if err != nil {
		return nil, info, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	bounds := img.Bounds()
	if crop != nil {
		r := image.Rect(crop.X, crop.Y, crop.X+crop.W, crop.Y+crop.H).Add(bounds.Min)
		if crop.W < 1 || crop.H < 1 || crop.X < 0 || crop.Y < 0 || !r.In(bounds) {
			return nil, info, fmt.Errorf("%w: %+v in %dx%d", ErrCrop, *crop, bounds.Dx(), bounds.Dy())
		}
		bounds = r
	}
	w, h := bounds.Dx(), bounds.Dy()
	if short := min(w, h); short > TargetShortSide {
		w, h = w*TargetShortSide/short, h*TargetShortSide/short
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return nil, info, fmt.Errorf("encode jpeg: %w", err)
	}
	return out.Bytes(), info, nil
}

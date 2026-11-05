package process

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"io"
	"testing"

	"github.com/gen2brain/h265/heic"
)

func TestCachedHEICFinalOutput(t *testing.T) {
	p := cachedProcessor(t)
	sourceImage := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			sourceImage.SetRGBA(x, y, color.RGBA{60, 90, 120, 255})
		}
	}
	input := pngBytes(t, sourceImage)
	opens := 0
	src := Source{Key: "heic-cache", Version: "1", Open: func(context.Context) (io.ReadCloser, error) {
		opens++
		return io.NopCloser(bytes.NewReader(input)), nil
	}}
	opts := Options{Width: 15, Height: 15, Mode: Fit, Rotate: 90, Encode: EncodeOptions{Format: HEIC, Quality: 85}}
	var first, second bytes.Buffer
	r, err := p.TransformCached(context.Background(), &first, src, opts)
	if err != nil || r.CacheHit || r.Format != HEIC {
		t.Fatalf("generate %+v, %v", r, err)
	}
	src.Open = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("HEIC cache hit opened source") }
	r, err = p.TransformCached(context.Background(), &second, src, opts)
	if err != nil || !r.CacheHit || r.BytesWritten != int64(second.Len()) {
		t.Fatalf("cache read %+v, %v", r, err)
	}
	if opens != 1 || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("cached final HEIC bytes differ")
	}
	img, err := heic.Decode(&second, heic.Options{AutoRotate: true, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Size() != image.Pt(11, 15) || r.Width != 11 || r.Height != 15 {
		t.Fatalf("HEIC presented dimensions=%v, result=%+v", img.Bounds(), r)
	}
	c := color.NRGBAModel.Convert(img.At(5, 7)).(color.NRGBA)
	if c.R < 50 || c.R > 70 || c.G < 80 || c.G > 100 || c.B < 110 || c.B > 130 || c.A != 255 {
		t.Fatalf("cached HEIC pixels=%v", c)
	}
}

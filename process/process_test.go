package process

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gen2brain/jpegn"
	"github.com/gen2brain/vpx/webp"
)

func processor(t *testing.T, cfg Config) *Processor {
	t.Helper()
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}
func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func normalized(t *testing.T, o Options) Options {
	t.Helper()
	n, err := ValidateOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestGeometry(t *testing.T) {
	cases := []struct {
		name         string
		sw, sh       int
		opts         Options
		w, h, rw, rh int
	}{
		{"fit width", 101, 50, Options{Width: 50, Mode: Fit}, 50, 25, 50, 25},
		{"fit height", 101, 50, Options{Height: 25, Mode: Fit}, 51, 25, 51, 25},
		{"fit box", 100, 50, Options{Width: 40, Height: 40, Mode: Fit}, 40, 20, 40, 20},
		{"fit no upscale", 100, 50, Options{Width: 400, Height: 400, Mode: Fit}, 100, 50, 100, 50},
		{"fit minimum pixel", 100, 1, Options{Width: 1, Mode: Fit}, 1, 1, 1, 1},
		{"fill ceil", 101, 50, Options{Width: 40, Height: 30, Mode: Fill}, 40, 30, 61, 30},
		{"exact", 100, 50, Options{Width: 20, Height: 30}, 20, 30, 20, 30},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			g, err := plan(tt.sw, tt.sh, normalized(t, tt.opts), 100_000_000)
			if err != nil {
				t.Fatal(err)
			}
			if g.width != tt.w || g.height != tt.h || g.resizeWidth != tt.rw || g.resizeHeight != tt.rh {
				t.Fatalf("geometry %+v", g)
			}
		})
	}
	for _, mode := range []Mode{Exact, Fill} {
		if _, err := plan(10, 10, normalized(t, Options{Width: 11, Height: 10, Mode: mode}), 1000); err == nil {
			t.Fatalf("%s allowed upscale", mode)
		}
		if _, err := plan(10, 10, normalized(t, Options{Width: 11, Height: 10, Mode: mode, AllowUpscale: true}), 1000); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := plan(100, 1, normalized(t, Options{Width: 1, Height: 10, Mode: Fill, AllowUpscale: true}), 100); err == nil {
		t.Fatal("intermediate pixel cap ignored")
	}
	if _, err := scaleDimension(int(^uint(0)>>1), int(^uint(0)>>1), 1, true); err == nil {
		t.Fatal("overflow accepted")
	}
}
func TestFillAnchors(t *testing.T) {
	// A 6x2 source is resized without scaling then cropped horizontally.
	src := image.NewRGBA(image.Rect(0, 0, 6, 2))
	for y := range 2 {
		for x := range 6 {
			src.SetRGBA(x, y, color.RGBA{R: uint8(20 + x*30), A: 255})
		}
	}
	encoded := pngBytes(t, src)
	p := processor(t, Config{})
	for _, a := range []Anchor{TopLeft, Top, TopRight, Left, Center, Right, BottomLeft, Bottom, BottomRight} {
		t.Run(string(a), func(t *testing.T) {
			var out bytes.Buffer
			_, err := p.Transform(context.Background(), &out, bytes.NewBuffer(encoded), Options{Width: 2, Height: 2, Mode: Fill, Anchor: a})
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(&out)
			if err != nil {
				t.Fatal(err)
			}
			x := 2
			switch a {
			case TopLeft, Left, BottomLeft:
				x = 0
			case TopRight, Right, BottomRight:
				x = 4
			}
			if got := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); got != src.RGBAAt(x, 0) {
				t.Fatalf("anchor pixel %v want %v", got, src.RGBAAt(x, 0))
			}
			// Vertical offsets exercise every row component independently.
			g, err := plan(2, 6, normalized(t, Options{Width: 2, Height: 2, Mode: Fill, Anchor: a}), 100)
			if err != nil {
				t.Fatal(err)
			}
			y := 2
			switch a {
			case TopLeft, Top, TopRight:
				y = 0
			case BottomLeft, Bottom, BottomRight:
				y = 4
			}
			if g.y != y {
				t.Fatalf("vertical anchor %d want %d", g.y, y)
			}
		})
	}
}
func TestCodecsAndJPEGFlatten(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			src.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 128})
		}
	}
	input := pngBytes(t, src)
	p := processor(t, Config{})
	for _, format := range []Format{PNG, JPEG, WebP} {
		t.Run(string(format), func(t *testing.T) {
			var out bytes.Buffer
			result, err := p.Transform(context.Background(), &out, bytes.NewBuffer(input), Options{Width: 8, Height: 8, Encode: EncodeOptions{Format: format, WebPLossless: true}})
			if err != nil {
				t.Fatal(err)
			}
			if result.BytesWritten != int64(out.Len()) || result.Width != 8 || result.Height != 8 || result.Format != format {
				t.Fatalf("result %+v", result)
			}
			data := append([]byte(nil), out.Bytes()...)
			var img image.Image
			switch format {
			case PNG:
				img, err = png.Decode(&out)
			case JPEG:
				img, err = jpegn.Decode(&out, &jpegn.Options{ToRGBA: true})
			case WebP:
				img, err = webp.Decode(&out, webp.Options{ToRGBA: true, Threads: 1})
			}
			if err != nil {
				t.Fatal(err)
			}
			c := color.NRGBAModel.Convert(img.At(3, 3)).(color.NRGBA)
			if format == JPEG {
				if c.R < 245 || c.G < 115 || c.G > 140 || c.B < 115 || c.B > 140 || c.A != 255 {
					t.Fatalf("incorrect JPEG white flatten: %v", c)
				}
			} else if c.A < 126 || c.A > 130 {
				t.Fatalf("alpha lost: %v", c)
			}
			// Exercise config replay for every input codec using a non-seekable reader.
			var again bytes.Buffer
			if _, err = p.Transform(context.Background(), &again, bytes.NewBuffer(data), Options{Width: 4, Height: 4, JPEGShrink: format == JPEG}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestLimitsAndValidation(t *testing.T) {
	input := pngBytes(t, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	for _, cfg := range []Config{{MaxInputBytes: int64(len(input) - 1)}, {MaxSourcePixels: 63}, {MaxOutputPixels: 15}} {
		p := processor(t, cfg)
		var out bytes.Buffer
		if _, err := p.Transform(context.Background(), &out, bytes.NewBuffer(input), Options{Width: 4, Height: 4}); err == nil {
			t.Fatalf("limit ignored: %+v", cfg)
		}
		if out.Len() != 0 {
			t.Fatal("wrote output before checking limits")
		}
	}
	p := processor(t, Config{MaxInputBytes: int64(len(input))})
	if _, err := p.Transform(context.Background(), io.Discard, bytes.NewBuffer(input), Options{Width: 4, Height: 4}); err != nil {
		t.Fatal(err)
	}
	trailing := append(append([]byte(nil), input...), 0)
	if _, err := p.Transform(context.Background(), io.Discard, bytes.NewBuffer(trailing), Options{Width: 4, Height: 4}); !errors.Is(err, errInputLimit) {
		t.Fatalf("trailing bound: %v", err)
	}
	for _, cfg := range []Config{{Concurrency: -1}, {Workers: -1}, {MaxInputBytes: -1}, {MaxSourcePixels: -1}, {MaxOutputPixels: -1}} {
		if p, err := New(cfg); err == nil {
			p.Close()
			t.Fatalf("negative config accepted %+v", cfg)
		}
	}
	for _, o := range []Options{{}, {Width: 1, Height: 1, Mode: "bad"}, {Width: 1, Height: 1, Encode: EncodeOptions{Format: JPEG, Background: color.NRGBA{R: 1, A: 128}}}, {Width: 1, Height: 1, Encode: EncodeOptions{WebPMethod: 7}}} {
		if _, err := ValidateOptions(o); err == nil {
			t.Fatalf("invalid options accepted %+v", o)
		}
	}
}

type blockingReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	r       io.Reader
}

func (r *blockingReader) Read(b []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.r.Read(b)
}
func TestCancellationAndClose(t *testing.T) {
	p := processor(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Transform(ctx, io.Discard, bytes.NewBuffer(nil), Options{Width: 1, Height: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	input := pngBytes(t, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	r := &blockingReader{entered: make(chan struct{}), release: make(chan struct{}), r: bytes.NewBuffer(input)}
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(r.release) }) }
	defer release()
	active := make(chan error, 1)
	go func() {
		_, err := p.Transform(context.Background(), io.Discard, r, Options{Width: 1, Height: 1})
		active <- err
	}()
	<-r.entered
	ctx, cancel = context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() {
		_, err := p.Transform(ctx, io.Discard, bytes.NewBuffer(input), Options{Width: 1, Height: 1})
		waiting <- err
	}()
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	// Synchronize with Close's state change without scheduling assumptions.
	deadline := time.After(5 * time.Second)
	for {
		p.mu.Lock()
		closing := p.closed
		p.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Close did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-closed:
		t.Fatal("Close did not drain active call")
	default:
	}
	if _, err := p.Transform(context.Background(), io.Discard, bytes.NewBuffer(input), Options{Width: 1, Height: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	release()
	if err := <-active; err != nil {
		t.Fatal(err)
	}
	<-closed
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

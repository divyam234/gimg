package process

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"runtime"
	"sync"

	resize "github.com/divyam234/gimg"
	"github.com/divyam234/gimg/diskcache"
)

type Mode string

const (
	Exact Mode = "exact"
	Fit   Mode = "fit"
	Fill  Mode = "fill"
)

type Anchor string

const (
	Center      Anchor = "center"
	TopLeft     Anchor = "top-left"
	Top         Anchor = "top"
	TopRight    Anchor = "top-right"
	Left        Anchor = "left"
	Right       Anchor = "right"
	BottomLeft  Anchor = "bottom-left"
	Bottom      Anchor = "bottom"
	BottomRight Anchor = "bottom-right"
)

type Format string

const (
	JPEG Format = "jpeg"
	PNG  Format = "png"
	WebP Format = "webp"
	HEIC Format = "heic"
)

// Config bounds concurrent jobs and resource consumption. Zero fields select
// one concurrent job, GOMAXPROCS workers/job, 64 MiB input, and 100M pixels.
// Cache is optional and is not owned or closed by the Processor.
type Config struct {
	Concurrency, Workers                            int
	MaxInputBytes, MaxSourcePixels, MaxOutputPixels int64
	Cache                                           *diskcache.Cache
}

// EncodeOptions selects the output codec. Quality defaults to 85; WebPMethod
// zero is valid. A zero Background selects opaque white; otherwise JPEG requires
// an opaque background. PNG, WebP, and HEIC preserve alpha.
type EncodeOptions struct {
	Format       Format
	Quality      int
	WebPLossless bool
	WebPMethod   int
	Background   color.NRGBA
}
type Options struct {
	Width, Height int
	Mode          Mode
	Filter        resize.Filter
	AllowUpscale  bool
	Anchor        Anchor
	Encode        EncodeOptions
	JPEGShrink    bool
	// DisableAutoOrient ignores EXIF orientation and HEIF rotation/mirroring.
	DisableAutoOrient bool
	// Rotate is clockwise degrees, restricted to multiples of 90.
	Rotate int
	// Flips use axes after source presentation and Rotate, before resize/crop.
	FlipHorizontal, FlipVertical bool
}
type Result struct {
	Width, Height int
	Format        Format
	BytesWritten  int64
	CacheHit      bool
}

var ErrClosed = errors.New("process: processor is closed")

// Processor owns a fixed pool of reusable resizers. It must not be copied.
type Processor struct {
	cfg     Config
	pool    chan *resize.Resizer
	mu      sync.Mutex
	closed  bool
	active  sync.WaitGroup
	done    chan struct{}
	cacheMu sync.Mutex
	flights map[string]*cacheFlight
}

func New(cfg Config) (*Processor, error) {
	if cfg.Concurrency < 0 || cfg.Workers < 0 || cfg.MaxInputBytes < 0 || cfg.MaxSourcePixels < 0 || cfg.MaxOutputPixels < 0 {
		return nil, errors.New("process: configuration limits cannot be negative")
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 1
	}
	if cfg.Workers == 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	if cfg.MaxInputBytes == 0 {
		cfg.MaxInputBytes = 64 << 20
	}
	if cfg.MaxSourcePixels == 0 {
		cfg.MaxSourcePixels = 100_000_000
	}
	if cfg.MaxOutputPixels == 0 {
		cfg.MaxOutputPixels = 100_000_000
	}
	p := &Processor{cfg: cfg, pool: make(chan *resize.Resizer, cfg.Concurrency), done: make(chan struct{})}
	for range cfg.Concurrency {
		p.pool <- resize.New(cfg.Workers)
	}
	return p, nil
}
func (p *Processor) begin(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	p.active.Add(1)
	return p.active.Done, nil
}

// Close rejects new calls, waits for active calls (including pool waiters), and
// releases workers. It is safe to call concurrently and repeatedly.
func (p *Processor) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.done
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.active.Wait()
	for range cap(p.pool) {
		(<-p.pool).Close()
	}
	close(p.done)
	return nil
}

// ValidateOptions returns canonical defaults, rejecting invalid options. Fields
// irrelevant to the selected mode or encoder are cleared after validation.
func ValidateOptions(o Options) (Options, error) {
	if o.Mode == "" {
		o.Mode = Exact
	}
	if o.Width < 0 || o.Height < 0 {
		return o, errors.New("process: dimensions cannot be negative")
	}
	switch o.Mode {
	case Exact, Fill:
		if o.Width == 0 || o.Height == 0 {
			return o, errors.New("process: exact and fill require both dimensions")
		}
	case Fit:
		if o.Width == 0 && o.Height == 0 {
			return o, errors.New("process: fit requires a dimension")
		}
	default:
		return o, fmt.Errorf("process: invalid mode %q", o.Mode)
	}
	if o.Filter != resize.Bicubic && o.Filter != resize.Lanczos3 {
		return o, resize.ErrInvalidFilter
	}
	if o.Rotate%90 != 0 {
		return o, errors.New("process: rotation must be a multiple of 90 degrees")
	}
	o.Rotate = (o.Rotate%360 + 360) % 360
	if o.Anchor == "" {
		o.Anchor = Center
	}
	switch o.Anchor {
	case Center, TopLeft, Top, TopRight, Left, Right, BottomLeft, Bottom, BottomRight:
	default:
		return o, fmt.Errorf("process: invalid anchor %q", o.Anchor)
	}
	if o.Mode != Fill {
		o.Anchor = Center
	}
	e := &o.Encode
	if e.Format == "" {
		e.Format = PNG
	}
	if e.Format != PNG && e.Format != JPEG && e.Format != WebP && e.Format != HEIC {
		return o, fmt.Errorf("process: invalid output format %q", e.Format)
	}
	if e.Quality < 0 || e.Quality > 100 {
		return o, errors.New("process: quality must be 1–100 or zero for default")
	}
	if e.Quality == 0 {
		e.Quality = 85
	}
	if e.WebPMethod < 0 || e.WebPMethod > 6 {
		return o, errors.New("process: WebP method must be 0–6")
	}
	if e.Format == JPEG {
		if e.Background == (color.NRGBA{}) {
			e.Background = color.NRGBA{255, 255, 255, 255}
		}
		if e.Background.A != 255 {
			return o, errors.New("process: JPEG background must be opaque")
		}
	} else {
		e.Background = color.NRGBA{}
	}
	if e.Format != WebP {
		e.WebPLossless = false
		e.WebPMethod = 0
	}
	if e.Format == PNG || (e.Format == WebP && e.WebPLossless) {
		e.Quality = 0
	}
	return o, nil
}
func (p *Processor) Transform(ctx context.Context, dst io.Writer, src io.Reader, opts Options) (Result, error) {
	release, err := p.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	opts, err = ValidateOptions(opts)
	if err != nil {
		return Result{}, err
	}
	return p.transform(ctx, dst, src, opts)
}

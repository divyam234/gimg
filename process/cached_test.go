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
	"sync/atomic"
	"testing"

	"github.com/divyam234/gimg/diskcache"
)

func cacheFixture(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			img.SetNRGBA(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func cachedProcessor(t *testing.T) *Processor {
	t.Helper()
	cache, err := diskcache.Open(diskcache.Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	p, err := New(Config{Concurrency: 2, Workers: 1, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func TestCachedSourceVersionAndNormalizedOptions(t *testing.T) {
	p := cachedProcessor(t)
	red := cacheFixture(t, color.NRGBA{R: 255, A: 255})
	blue := cacheFixture(t, color.NRGBA{B: 255, A: 255})
	var opens atomic.Int32
	src := Source{Key: "photo", Version: "v1", Open: func(context.Context) (io.ReadCloser, error) {
		opens.Add(1)
		return io.NopCloser(bytes.NewReader(red)), nil
	}}
	opts := Options{Width: 10, Height: 5, Encode: EncodeOptions{Format: PNG}}
	var first, second bytes.Buffer
	r, err := p.TransformCached(context.Background(), &first, src, opts)
	if err != nil || r.CacheHit {
		t.Fatalf("initial transform: %+v, %v", r, err)
	}
	// Irrelevant crop and JPEG quality options must not split PNG exact entries.
	opts.Anchor = BottomRight
	opts.Encode.Quality = 20
	src.Open = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("cache hit opened source") }
	r, err = p.TransformCached(context.Background(), &second, src, opts)
	if err != nil || !r.CacheHit || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("cache hit: %+v, %v", r, err)
	}
	if opens.Load() != 1 {
		t.Fatalf("opens=%d", opens.Load())
	}
	src.Version = "v2"
	src.Open = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(blue)), nil }
	var changed bytes.Buffer
	r, err = p.TransformCached(context.Background(), &changed, src, opts)
	if err != nil || r.CacheHit {
		t.Fatalf("new version: %+v, %v", r, err)
	}
	img, err := png.Decode(&changed)
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(img.At(5, 2)).(color.NRGBA); got != (color.NRGBA{B: 255, A: 255}) {
		t.Fatalf("version returned stale pixels: %+v", got)
	}
}

func TestCachedConcurrentMissAndCanceledWaiter(t *testing.T) {
	p := cachedProcessor(t)
	data := cacheFixture(t, color.NRGBA{G: 255, A: 255})
	started, proceed := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(proceed) }) }
	defer unblock()
	var opens atomic.Int32
	src := Source{Key: "shared", Version: "1", Open: func(context.Context) (io.ReadCloser, error) {
		if opens.Add(1) == 1 {
			close(started)
		}
		<-proceed
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
	opts := Options{Width: 10, Height: 5}
	leader := make(chan error, 1)
	go func() { _, err := p.TransformCached(context.Background(), io.Discard, src, opts); leader <- err }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.TransformCached(ctx, io.Discard, src, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var dst bytes.Buffer
			_, err := p.TransformCached(context.Background(), &dst, src, opts)
			if err == nil {
				var img image.Image
				img, err = png.Decode(&dst)
				if err == nil && img.Bounds().Size() != image.Pt(10, 5) {
					err = errors.New("incorrect shared dimensions")
				}
			}
			errs <- err
		}()
	}
	unblock()
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if opens.Load() != 1 {
		t.Fatalf("concurrent misses opened source %d times", opens.Load())
	}
}

type failingCacheDestination struct{ err error }

func (w failingCacheDestination) Write([]byte) (int, error) { return 0, w.err }

type closeErrorReader struct {
	io.Reader
	err error
}

func (r closeErrorReader) Close() error { return r.err }

func TestCachedPublicationAndCallerWriteFailures(t *testing.T) {
	p := cachedProcessor(t)
	data := cacheFixture(t, color.NRGBA{R: 255, A: 255})
	opts := Options{Width: 10, Height: 5}
	closeErr := errors.New("source close failure")
	src := Source{Key: "errors", Version: "1", Open: func(context.Context) (io.ReadCloser, error) {
		return closeErrorReader{Reader: bytes.NewReader(data), err: closeErr}, nil
	}}
	if _, err := p.TransformCached(context.Background(), io.Discard, src, opts); !errors.Is(err, closeErr) {
		t.Fatalf("source close: %v", err)
	}
	src.Open = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	writeErr := errors.New("consumer disconnected")
	r, err := p.TransformCached(context.Background(), failingCacheDestination{writeErr}, src, opts)
	if !errors.Is(err, writeErr) || r.CacheHit {
		t.Fatalf("destination failure: %+v, %v", r, err)
	}
	src.Open = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("published result missing") }
	var dst bytes.Buffer
	r, err = p.TransformCached(context.Background(), &dst, src, opts)
	if err != nil || !r.CacheHit {
		t.Fatalf("published result: %+v, %v", r, err)
	}
	img, err := png.Decode(&dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(img.At(5, 2)).(color.NRGBA); got != (color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("published pixels: %+v", got)
	}
}

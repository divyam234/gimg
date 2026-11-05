package process

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/divyam234/gimg/diskcache"
)

// Source identifies immutable encoded image bytes. Change Version whenever the
// bytes change. Key and Version must be nonempty. Open is called only on a miss;
// the processor closes the returned reader. Source retrieval remains app-owned.
type Source struct {
	Key     string
	Version string
	Open    func(context.Context) (io.ReadCloser, error)
}

var (
	// ErrCache identifies cache lookup, storage, or publication failures.
	ErrCache = errors.New("process: cache failure")
	// ErrCacheUnavailable means TransformCached has no configured disk cache.
	ErrCacheUnavailable = errors.New("process: no disk cache configured")
	// ErrInvalidSource means a cacheable source lacks identity or an opener.
	ErrInvalidSource = errors.New("process: invalid cacheable source")
)

type cacheFlight struct {
	done chan struct{}
	err  error
}

// TransformCached transforms an immutable source, caching its encoded result.
// Cache hits do not open the source. Concurrent misses for the same key in this
// processor share generation; the generating caller's cancellation also fails
// its waiters. Another call may retry. Input/source limits apply on generation;
// cached output dimensions are still checked against the output pixel limit.
// Cache errors are returned, not silently bypassed. A failed destination write
// does not invalidate a successfully published entry. The destination stays open.
func (p *Processor) TransformCached(ctx context.Context, dst io.Writer, src Source, opts Options) (Result, error) {
	release, err := p.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if dst == nil || src.Key == "" || src.Version == "" || src.Open == nil {
		return Result{}, ErrInvalidSource
	}
	opts, err = ValidateOptions(opts)
	if err != nil {
		return Result{}, err
	}
	if p.cfg.Cache == nil {
		return Result{}, ErrCacheUnavailable
	}
	key, err := cacheKey(src, opts)
	if err != nil {
		return Result{}, err
	}
lookup:
	entry, err := p.cfg.Cache.Get(ctx, key)
	if err == nil {
		return p.copyCached(ctx, dst, entry, true)
	}
	if !errors.Is(err, diskcache.ErrMiss) {
		return Result{}, fmt.Errorf("%w: lookup: %w", ErrCache, err)
	}

	p.cacheMu.Lock()
	if f := p.flights[key]; f != nil {
		p.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-f.done:
		}
		if f.err != nil {
			return Result{}, f.err
		}
		entry, err := p.cfg.Cache.Get(ctx, key)
		if errors.Is(err, diskcache.ErrMiss) {
			goto lookup
		}
		if err != nil {
			return Result{}, fmt.Errorf("%w: shared result: %w", ErrCache, err)
		}
		return p.copyCached(ctx, dst, entry, true)
	}
	if p.flights == nil {
		p.flights = make(map[string]*cacheFlight)
	}
	f := &cacheFlight{done: make(chan struct{})}
	p.flights[key] = f
	p.cacheMu.Unlock()
	// Always publish generation completion before copying to a caller-owned
	// writer: a slow or broken consumer must not stall other cache users.
	entry, generatedHit, err := p.generateCached(ctx, key, src, opts)
	p.cacheMu.Lock()
	f.err = err
	delete(p.flights, key)
	close(f.done)
	p.cacheMu.Unlock()
	if err != nil {
		return Result{}, err
	}
	return p.copyCached(ctx, dst, entry, generatedHit)
}

func (p *Processor) generateCached(ctx context.Context, key string, src Source, opts Options) (entry *diskcache.Entry, hit bool, err error) {
	// Close the race between the initial miss and becoming the generator.
	entry, err = p.cfg.Cache.Get(ctx, key)
	if err == nil {
		return entry, true, nil
	}
	if !errors.Is(err, diskcache.ErrMiss) {
		return nil, false, fmt.Errorf("%w: lookup: %w", ErrCache, err)
	}
	resizer, release, err := p.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	reader, err := src.Open(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("process: open source: %w", err)
	}
	if reader == nil {
		return nil, false, fmt.Errorf("%w: opener returned nil reader", ErrInvalidSource)
	}
	writer, err := p.cfg.Cache.Create(ctx, key)
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("%w: create: %w", ErrCache, err), reader.Close())
	}
	defer func() {
		if abortErr := writer.Close(); abortErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: abort: %w", ErrCache, abortErr))
		}
	}()
	result, transformErr := p.transformWithResizer(ctx, cacheStorageWriter{writer}, reader, opts, resizer)
	if err := errors.Join(transformErr, reader.Close()); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	entry, err = writer.CommitAndOpen(diskcache.Metadata{Width: result.Width, Height: result.Height, Format: string(result.Format)})
	if err != nil {
		return nil, false, fmt.Errorf("%w: publish: %w", ErrCache, err)
	}
	return entry, false, nil
}

func (p *Processor) copyCached(ctx context.Context, dst io.Writer, entry *diskcache.Entry, hit bool) (Result, error) {
	m := entry.Metadata
	result := Result{Width: m.Width, Height: m.Height, Format: Format(m.Format), CacheHit: hit}
	if m.Width <= 0 || m.Height <= 0 || int64(m.Width) > p.cfg.MaxOutputPixels/int64(m.Height) {
		return result, errors.Join(fmt.Errorf("process: cached output exceeds pixel limit"), cacheCloseError(entry.Close()))
	}
	if result.Format != JPEG && result.Format != PNG && result.Format != WebP && result.Format != HEIC {
		return result, errors.Join(fmt.Errorf("%w: invalid cached format", ErrCache), cacheCloseError(entry.Close()))
	}
	var copyErr error
	result.BytesWritten, copyErr = io.Copy(cacheContextWriter{ctx: ctx, dst: dst}, cacheContextReader{ctx: ctx, src: entry})
	return result, errors.Join(copyErr, cacheCloseError(entry.Close()))
}

func cacheCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: close: %w", ErrCache, err)
}

type cacheContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r cacheContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.src.Read(b)
	if err != nil && err != io.EOF {
		err = fmt.Errorf("%w: read: %w", ErrCache, err)
	}
	return n, err
}

type cacheContextWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w cacheContextWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.dst.Write(b)
}

type cacheStorageWriter struct{ io.Writer }

func (w cacheStorageWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	if err != nil {
		return n, fmt.Errorf("%w: write: %w", ErrCache, err)
	}
	return n, nil
}

func cacheKey(src Source, opts Options) (string, error) {
	encoded, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("process: encode cache options: %w", err)
	}
	h := sha256.New()
	var size [8]byte
	for _, part := range []string{src.Key, src.Version} {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		io.WriteString(h, part)
	}
	binary.BigEndian.PutUint64(size[:], uint64(len(encoded)))
	h.Write(size[:])
	h.Write(encoded)
	return hex.EncodeToString(h.Sum(nil)), nil
}

package diskcache

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrMiss        = errors.New("diskcache: cache miss")
	ErrClosed      = errors.New("diskcache: closed")
	ErrInvalidKey  = errors.New("diskcache: invalid key")
	errUnsafeEntry = errors.New("diskcache: not a regular entry")
)

const (
	headerSize   = 64
	entrySuffix  = ".entry"
	tempPrefix   = ".gimg-"
	tempSuffix   = ".tmp"
	staleTempAge = 24 * time.Hour
)

var magic = [8]byte{'I', 'M', 'G', 'C', 'A', 'C', 'H', 1}

// Config selects the directory and optional creation-time and storage limits.
type Config struct {
	Directory       string
	MaxAge          time.Duration
	MaxBytes        int64
	CleanupInterval time.Duration
}

// Metadata describes an encoded image; Commit assigns Created and Size.
type Metadata struct {
	Width, Height int
	Format        string
	Created       time.Time
	Size          int64
}

// Entry is a caller-owned payload reader with validated metadata.
type Entry struct {
	io.ReadCloser
	Metadata Metadata
}

// PruneResult counts removed files and their logical sizes, including headers.
type PruneResult struct {
	Removed    int
	BytesFreed int64
}

// Cache is safe for concurrent use. Close aborts unpublished writers only.
type Cache struct {
	cfg            Config
	root           *os.Root
	mu             sync.Mutex
	fsMu           sync.Mutex
	closed         bool
	writers        map[*Writer]struct{}
	ops            sync.WaitGroup
	stop           chan struct{}
	backgroundDone chan struct{}
	closeDone      chan struct{}
	closeErr       error
	cleanupErr     error
}

// Open creates the directory if necessary and starts optional background cleanup.
func Open(cfg Config) (*Cache, error) {
	if cfg.Directory == "" || cfg.MaxAge < 0 || cfg.MaxBytes < 0 || cfg.CleanupInterval < 0 {
		return nil, fmt.Errorf("diskcache open: directory required and limits must be nonnegative")
	}
	if err := os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, fmt.Errorf("diskcache open: %w", err)
	}
	root, err := os.OpenRoot(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("diskcache open: %w", err)
	}
	c := &Cache{cfg: cfg, root: root, writers: make(map[*Writer]struct{}), stop: make(chan struct{}), backgroundDone: make(chan struct{}), closeDone: make(chan struct{})}
	if cfg.CleanupInterval == 0 {
		close(c.backgroundDone)
	} else {
		go c.cleanup()
	}
	return c, nil
}

func validKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, b := range []byte(key) {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

func (c *Cache) begin() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	c.ops.Add(1)
	return c.ops.Done, nil
}

func (c *Cache) cleanup() {
	defer close(c.backgroundDone)
	ticker := time.NewTicker(c.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			_, err := c.Prune(context.Background())
			c.mu.Lock()
			if !c.closed {
				c.cleanupErr = err
			}
			c.mu.Unlock()
		}
	}
}

// LastCleanupError returns the outcome of the most recent background prune.
// A successful subsequent cleanup clears an earlier error.
func (c *Cache) LastCleanupError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanupErr
}

func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	close(c.stop)
	c.mu.Unlock()
	<-c.backgroundDone
	c.ops.Wait()
	c.mu.Lock()
	writers := make([]*Writer, 0, len(c.writers))
	for w := range c.writers {
		writers = append(writers, w)
	}
	c.mu.Unlock()
	var err error
	for _, w := range writers {
		err = errors.Join(err, w.Close())
	}
	err = errors.Join(err, c.root.Close())
	c.closeErr = err
	close(c.closeDone)
	return err
}

func decodeHeader(f *os.File) (Metadata, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return Metadata{}, err
	}
	if string(h[:8]) != string(magic[:]) {
		return Metadata{}, errors.New("invalid header magic")
	}
	width, height := binary.BigEndian.Uint64(h[8:16]), binary.BigEndian.Uint64(h[16:24])
	sec := int64(binary.BigEndian.Uint64(h[24:32]))
	nanos := binary.BigEndian.Uint32(h[32:36])
	size := int64(binary.BigEndian.Uint64(h[40:48]))
	format := ""
	switch h[36] {
	case 1:
		format = "jpeg"
	case 2:
		format = "png"
	case 3:
		format = "webp"
	case 4:
		format = "heic"
	}
	if width == 0 || height == 0 || width > uint64(^uint(0)>>1) || height > uint64(^uint(0)>>1) || sec <= 0 || nanos >= 1e9 || size <= 0 || format == "" {
		return Metadata{}, errors.New("invalid metadata")
	}
	for _, b := range h[37:40] {
		if b != 0 {
			return Metadata{}, errors.New("invalid reserved header")
		}
	}
	for _, b := range h[48:] {
		if b != 0 {
			return Metadata{}, errors.New("invalid reserved header")
		}
	}
	info, err := f.Stat()
	if err != nil {
		return Metadata{}, err
	}
	if info.Size() < headerSize || info.Size()-headerSize != size {
		return Metadata{}, errors.New("payload size mismatch")
	}
	return Metadata{Width: int(width), Height: int(height), Format: format, Created: time.Unix(sec, int64(nanos)).UTC(), Size: size}, nil
}

func expired(m Metadata, now time.Time, age time.Duration) bool {
	return age > 0 && !now.Before(m.Created.Add(age))
}

func (c *Cache) Get(ctx context.Context, key string) (*Entry, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("diskcache get: %w", ErrInvalidKey)
	}
	done, err := c.begin()
	if err != nil {
		return nil, fmt.Errorf("diskcache get: %w", err)
	}
	defer done()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diskcache get: %w", err)
	}
	c.fsMu.Lock()
	defer c.fsMu.Unlock()
	name := key + entrySuffix
	f, err := c.openRegular(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsafeEntry) {
			return nil, ErrMiss
		}
		return nil, fmt.Errorf("diskcache get: %w", err)
	}
	m, err := decodeHeader(f)
	if err != nil || expired(m, time.Now(), c.cfg.MaxAge) {
		f.Close()
		if removeErr := c.unlink(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, fmt.Errorf("diskcache get removing invalid entry: %w", removeErr)
		}
		return nil, ErrMiss
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, fmt.Errorf("diskcache get: %w", err)
	}
	return &Entry{ReadCloser: &entryReader{file: f, ctx: ctx}, Metadata: m}, nil
}

type entryReader struct {
	file *os.File
	ctx  context.Context
}

func (r *entryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(p)
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}
func (r *entryReader) Close() error { return r.file.Close() }

// Writer is an unpublished entry. Close aborts it; Commit publishes it once.
// Cache.Close aborts all remaining writers. A Writer serializes its methods.
// Metadata.Created and Metadata.Size are assigned by Commit, not the caller.
type Writer struct {
	mu        sync.Mutex
	cache     *Cache
	ctx       context.Context
	file      *os.File
	name, key string
	size      int64
	closed    bool
}

func (c *Cache) Create(ctx context.Context, key string) (*Writer, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("diskcache create: %w", ErrInvalidKey)
	}
	done, err := c.begin()
	if err != nil {
		return nil, fmt.Errorf("diskcache create: %w", err)
	}
	defer done()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diskcache create: %w", err)
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("diskcache create: %w", err)
	}
	name := tempPrefix + hex.EncodeToString(token[:]) + tempSuffix
	f, err := c.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("diskcache create: %w", err)
	}
	if _, err = f.Write(make([]byte, headerSize)); err != nil {
		f.Close()
		c.unlink(name)
		return nil, fmt.Errorf("diskcache create header: %w", err)
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		c.unlink(name)
		return nil, fmt.Errorf("diskcache create: %w", err)
	}
	w := &Writer{cache: c, ctx: ctx, file: f, name: name, key: key}
	c.mu.Lock()
	c.writers[w] = struct{}{}
	c.mu.Unlock()
	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("diskcache write: %w", ErrClosed)
	}
	done, err := w.cache.begin()
	if err != nil {
		return 0, fmt.Errorf("diskcache write: %w", err)
	}
	defer done()
	if err := w.ctx.Err(); err != nil {
		return 0, fmt.Errorf("diskcache write: %w", err)
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err == nil {
		err = w.ctx.Err()
	}
	if err != nil {
		return n, fmt.Errorf("diskcache write: %w", err)
	}
	return n, err
}

// Commit atomically publishes the completed entry. Close afterward is safe.
func (w *Writer) Commit(m Metadata) error {
	_, err := w.commit(m, false)
	return err
}

// CommitAndOpen publishes the completed entry and returns a payload reader
// opened before publication. The reader remains usable even if another process
// immediately replaces or prunes the published filename. The caller must close
// it. The writer's context also governs reads from the returned Entry.
func (w *Writer) CommitAndOpen(m Metadata) (*Entry, error) {
	return w.commit(m, true)
}

func (w *Writer) commit(m Metadata, open bool) (*Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, fmt.Errorf("diskcache commit: %w", ErrClosed)
	}
	done, err := w.cache.begin()
	if err != nil {
		return nil, fmt.Errorf("diskcache commit: %w", err)
	}
	defer done()
	if err := w.ctx.Err(); err != nil {
		return nil, fmt.Errorf("diskcache commit: %w", err)
	}
	var format byte
	switch m.Format {
	case "jpeg":
		format = 1
	case "png":
		format = 2
	case "webp":
		format = 3
	case "heic":
		format = 4
	}
	if m.Width <= 0 || m.Height <= 0 || format == 0 || w.size <= 0 {
		return nil, errors.New("diskcache commit: invalid metadata or empty payload")
	}
	info, err := w.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("diskcache commit: %w", err)
	}
	if info.Size()-headerSize != w.size {
		return nil, errors.New("diskcache commit: payload size mismatch")
	}
	var h [headerSize]byte
	copy(h[:8], magic[:])
	binary.BigEndian.PutUint64(h[8:16], uint64(m.Width))
	binary.BigEndian.PutUint64(h[16:24], uint64(m.Height))
	now := time.Now().UTC()
	binary.BigEndian.PutUint64(h[24:32], uint64(now.Unix()))
	binary.BigEndian.PutUint32(h[32:36], uint32(now.Nanosecond()))
	h[36] = format
	binary.BigEndian.PutUint64(h[40:48], uint64(w.size))
	if _, err := w.file.WriteAt(h[:], 0); err != nil {
		return nil, fmt.Errorf("diskcache commit header: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return nil, fmt.Errorf("diskcache commit sync: %w", err)
	}
	var entry *Entry
	defer func() {
		if entry != nil {
			entry.Close()
		}
	}()
	if open {
		f, err := w.cache.openRegular(w.name)
		if err != nil {
			return nil, fmt.Errorf("diskcache commit open: %w", err)
		}
		if _, err := f.Seek(headerSize, io.SeekStart); err != nil {
			f.Close()
			return nil, fmt.Errorf("diskcache commit seek: %w", err)
		}
		m.Created, m.Size = now, w.size
		entry = &Entry{ReadCloser: &entryReader{file: f, ctx: w.ctx}, Metadata: m}
	}
	w.cache.fsMu.Lock()
	err = w.ctx.Err()
	if err == nil {
		err = w.cache.root.Rename(w.name, w.key+entrySuffix)
	}
	w.cache.fsMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("diskcache commit publish: %w", err)
	}
	w.closed = true
	err = w.file.Close()
	w.unregister()
	if err != nil {
		return nil, fmt.Errorf("diskcache commit close: %w", err)
	}
	result := entry
	entry = nil
	return result, nil
}

func (w *Writer) unregister() {
	w.cache.mu.Lock()
	delete(w.cache.writers, w)
	w.cache.mu.Unlock()
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.file.Close()
	if removeErr := w.cache.unlink(w.name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	w.unregister()
	if err != nil {
		return fmt.Errorf("diskcache abort: %w", err)
	}
	return nil
}

func ownedEntry(name string) bool {
	return strings.HasSuffix(name, entrySuffix) && validKey(strings.TrimSuffix(name, entrySuffix))
}
func ownedTemp(name string) bool {
	return strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, tempSuffix) && validKey(strings.TrimSuffix(strings.TrimPrefix(name, tempPrefix), tempSuffix))
}

func (c *Cache) Prune(ctx context.Context) (PruneResult, error) {
	var result PruneResult
	done, err := c.begin()
	if err != nil {
		return result, fmt.Errorf("diskcache prune: %w", err)
	}
	defer done()
	c.fsMu.Lock()
	defer c.fsMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("diskcache prune: %w", err)
	}
	dir, err := c.root.Open(".")
	if err != nil {
		return result, fmt.Errorf("diskcache prune: %w", err)
	}
	defer dir.Close()
	names, err := dir.ReadDir(-1)
	if err != nil {
		return result, fmt.Errorf("diskcache prune: %w", err)
	}
	c.mu.Lock()
	active := make(map[string]bool, len(c.writers))
	for w := range c.writers {
		active[w.name] = true
	}
	c.mu.Unlock()
	type candidate struct {
		name    string
		created time.Time
		size    int64
	}
	var entries []candidate
	var total int64
	now := time.Now()
	remove := func(name string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.unlink(name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		result.Removed++
		result.BytesFreed += size
		return nil
	}
	for _, item := range names {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("diskcache prune: %w", err)
		}
		name := item.Name()
		if !ownedEntry(name) && !ownedTemp(name) {
			continue
		}
		info, err := c.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("diskcache prune: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if ownedTemp(name) {
			if !active[name] && !now.Before(info.ModTime().Add(staleTempAge)) {
				if err := remove(name, info.Size()); err != nil {
					return result, fmt.Errorf("diskcache prune temp: %w", err)
				}
			}
			continue
		}
		f, err := c.openRegular(name)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsafeEntry) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("diskcache prune: %w", err)
		}
		m, headerErr := decodeHeader(f)
		f.Close()
		if headerErr != nil || expired(m, now, c.cfg.MaxAge) {
			if err := remove(name, info.Size()); err != nil {
				return result, fmt.Errorf("diskcache prune entry: %w", err)
			}
			continue
		}
		total += info.Size()
		entries = append(entries, candidate{name, m.Created, info.Size()})
	}
	if c.cfg.MaxBytes > 0 && total > c.cfg.MaxBytes {
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].created.Equal(entries[j].created) {
				return entries[i].name < entries[j].name
			}
			return entries[i].created.Before(entries[j].created)
		})
		for _, entry := range entries {
			if total <= c.cfg.MaxBytes {
				break
			}
			if err := remove(entry.name, entry.size); err != nil {
				return result, fmt.Errorf("diskcache prune size: %w", err)
			}
			total -= entry.size
		}
	}
	return result, nil
}

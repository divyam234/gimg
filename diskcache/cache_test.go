package diskcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testContext = context.Background()

func testKey(n int) string { return fmt.Sprintf("%064x", n) }

func newTestCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	if cfg.Directory == "" {
		cfg.Directory = t.TempDir()
	}
	c, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func publish(t *testing.T, c *Cache, key, payload string) {
	t.Helper()
	w, err := c.Create(testContext, key)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(Metadata{Width: 20, Height: 10, Format: "png", Created: time.Unix(1, 0), Size: 999}); err != nil {
		t.Fatal(err)
	}
}

func setCreated(t *testing.T, c *Cache, key string, created time.Time) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(c.cfg.Directory, key+entrySuffix), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var stamp [12]byte
	binary.BigEndian.PutUint64(stamp[:8], uint64(created.Unix()))
	binary.BigEndian.PutUint32(stamp[8:], uint32(created.Nanosecond()))
	if _, err := f.WriteAt(stamp[:], 24); err != nil {
		t.Fatal(err)
	}
}

func requireMiss(t *testing.T, c *Cache, key string) {
	t.Helper()
	e, err := c.Get(testContext, key)
	if e != nil {
		e.Close()
		t.Fatal("unexpected entry")
	}
	if !errors.Is(err, ErrMiss) {
		t.Fatalf("got %v, want ErrMiss", err)
	}
}

func TestPublicationAbortAndMetadata(t *testing.T) {
	c := newTestCache(t, Config{})
	key := testKey(1)
	w, err := c.Create(testContext, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("unpublished")); err != nil {
		t.Fatal(err)
	}
	requireMiss(t, c, key)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	requireMiss(t, c, key)
	if _, err := os.Stat(filepath.Join(c.cfg.Directory, w.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted temp remains: %v", err)
	}
	if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "png"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("commit aborted writer: %v", err)
	}
	before := time.Now()
	publish(t, c, key, "encoded image")
	e, err := c.Get(testContext, key)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Metadata.Width != 20 || e.Metadata.Height != 10 || e.Metadata.Format != "png" || e.Metadata.Size != 13 || e.Metadata.Created.Before(before) {
		t.Fatalf("unexpected metadata: %+v", e.Metadata)
	}
	data, err := io.ReadAll(e)
	if err != nil || string(data) != "encoded image" {
		t.Fatalf("payload %q: %v", data, err)
	}
	publish(t, c, key, "replacement")
}

func TestCorruptEntriesAreRemovedWithoutReaders(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"truncated header":  func(b []byte) []byte { return b[:8] },
		"truncated payload": func(b []byte) []byte { return b[:len(b)-1] },
		"trailing payload":  func(b []byte) []byte { return append(b, 1) },
		"magic":             func(b []byte) []byte { b[0] = 0; return b },
		"width":             func(b []byte) []byte { clear(b[8:16]); return b },
		"height":            func(b []byte) []byte { clear(b[16:24]); return b },
		"format":            func(b []byte) []byte { b[36] = 255; return b },
		"created":           func(b []byte) []byte { clear(b[24:32]); return b },
		"nanoseconds":       func(b []byte) []byte { binary.BigEndian.PutUint32(b[32:36], 1e9); return b },
		"negative size":     func(b []byte) []byte { binary.BigEndian.PutUint64(b[40:48], ^uint64(0)); return b },
		"reserved":          func(b []byte) []byte { b[63] = 1; return b },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestCache(t, Config{})
			key := testKey(1)
			publish(t, c, key, "payload")
			path := filepath.Join(c.cfg.Directory, key+entrySuffix)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, corrupt(b), 0600); err != nil {
				t.Fatal(err)
			}
			requireMiss(t, c, key)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corruption not removed: %v", err)
			}
		})
	}
}

func TestTTLAndOldestCreatedByteEviction(t *testing.T) {
	c := newTestCache(t, Config{MaxAge: time.Hour, MaxBytes: 2 * (headerSize + 4)})
	for i := 1; i <= 4; i++ {
		publish(t, c, testKey(i), "data")
	}
	now := time.Now()
	setCreated(t, c, testKey(1), now.Add(-2*time.Hour))
	setCreated(t, c, testKey(2), now.Add(-30*time.Minute))
	setCreated(t, c, testKey(3), now.Add(-20*time.Minute))
	setCreated(t, c, testKey(4), now.Add(-10*time.Minute))
	// A read does not refresh the oldest surviving entry.
	e, err := c.Get(testContext, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	result, err := c.Prune(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 2 || result.BytesFreed != 2*(headerSize+4) {
		t.Fatalf("prune: %+v", result)
	}
	requireMiss(t, c, testKey(1))
	requireMiss(t, c, testKey(2))
	for _, i := range []int{3, 4} {
		e, err := c.Get(testContext, testKey(i))
		if err != nil {
			t.Fatal(err)
		}
		e.Close()
	}
	setCreated(t, c, testKey(3), now.Add(-2*time.Hour))
	requireMiss(t, c, testKey(3))
	if _, err := os.Stat(filepath.Join(c.cfg.Directory, testKey(3)+entrySuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired Get not removed: %v", err)
	}
}

func TestPrunePreservesUnrelatedAndActiveTemps(t *testing.T) {
	c := newTestCache(t, Config{})
	old := time.Now().Add(-48 * time.Hour)
	unrelated := []string{"notes", "wrong.entry", strings.Repeat("A", 64) + entrySuffix, tempPrefix + "wrong" + tempSuffix}
	stale := tempPrefix + testKey(40) + tempSuffix
	fresh := tempPrefix + testKey(41) + tempSuffix
	for _, name := range append(append([]string{}, unrelated...), stale, fresh) {
		path := filepath.Join(c.cfg.Directory, name)
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if name != fresh {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	ownedDir := testKey(42) + entrySuffix
	if err := os.Mkdir(filepath.Join(c.cfg.Directory, ownedDir), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := c.Create(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.Chtimes(filepath.Join(c.cfg.Directory, w.name), old, old); err != nil {
		t.Fatal(err)
	}
	// A corrupt strict-name entry is removed by explicit pruning as well.
	if err := os.WriteFile(filepath.Join(c.cfg.Directory, testKey(43)+entrySuffix), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := c.Prune(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 2 || result.BytesFreed != 7 {
		t.Fatalf("prune: %+v", result)
	}
	for _, name := range append(unrelated, fresh, ownedDir, w.name) {
		if _, err := os.Lstat(filepath.Join(c.cfg.Directory, name)); err != nil {
			t.Fatalf("removed protected %s: %v", name, err)
		}
	}
	if _, err := w.Write([]byte("still active")); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "jpeg"}); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	c := newTestCache(t, Config{})
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not touch"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.cfg.Directory, testKey(1)+entrySuffix)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	requireMiss(t, c, testKey(1))
	if _, err := c.Prune(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("prune removed symlink", err)
	}
	publish(t, c, testKey(1), "safe replacement")
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "do not touch" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
}

func TestCloseAbortsWritersButPreservesReaders(t *testing.T) {
	c := newTestCache(t, Config{CleanupInterval: time.Hour})
	publish(t, c, testKey(1), "reader survives")
	e, err := c.Get(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	w, err := c.Create(testContext, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := os.Stat(filepath.Join(c.cfg.Directory, w.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp remains: %v", err)
	}
	if _, err := w.Write([]byte("closed")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(testContext, testKey(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("get after close: %v", err)
	}
	if _, err := c.Create(testContext, testKey(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close: %v", err)
	}
	if _, err := c.Prune(testContext); !errors.Is(err, ErrClosed) {
		t.Fatalf("prune after close: %v", err)
	}
	data, err := io.ReadAll(e)
	if err != nil || string(data) != "reader survives" {
		t.Fatalf("reader after close: %q %v", data, err)
	}
}

func TestReaderSurvivesPruneAndReplacement(t *testing.T) {
	c := newTestCache(t, Config{MaxBytes: 1})
	publish(t, c, testKey(1), "original")
	e, err := c.Get(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	publish(t, c, testKey(1), "replacement")
	if _, err := c.Prune(testContext); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(e)
	if err != nil || string(data) != "original" {
		t.Fatalf("opened inode changed: %q %v", data, err)
	}
}

func TestValidationAndCancellation(t *testing.T) {
	for _, cfg := range []Config{{}, {Directory: t.TempDir(), MaxAge: -1}, {Directory: t.TempDir(), MaxBytes: -1}, {Directory: t.TempDir(), CleanupInterval: -1}} {
		if c, err := Open(cfg); err == nil {
			c.Close()
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
	c := newTestCache(t, Config{})
	for _, key := range []string{"", "../escape", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := c.Get(testContext, key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("invalid get key %q: %v", key, err)
		}
		if _, err := c.Create(testContext, key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("invalid create key %q: %v", key, err)
		}
	}
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if _, err := c.Get(ctx, testKey(1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, testKey(1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Prune(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(testContext)
	w, err := c.Create(ctx, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := w.Write([]byte("more")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "png"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	requireMiss(t, c, testKey(1))
	publish(t, c, testKey(2), "reader")
	ctx, cancel = context.WithCancel(testContext)
	e, err := c.Get(ctx, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cancel()
	if _, err := e.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCommitRejectsInvalidMetadataAndPayloadMutation(t *testing.T) {
	c := newTestCache(t, Config{})
	for _, metadata := range []Metadata{{}, {Width: 1, Height: 1, Format: "gif"}, {Width: -1, Height: 1, Format: "png"}} {
		w, err := c.Create(testContext, testKey(1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(metadata); err == nil {
			t.Fatalf("accepted metadata: %+v", metadata)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		requireMiss(t, c, testKey(1))
	}
	w, err := c.Create(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "png"}); err == nil {
		t.Fatal("accepted empty payload")
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := w.file.Truncate(headerSize + 2); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "png"}); err == nil {
		t.Fatal("accepted truncated temporary payload")
	}
	requireMiss(t, c, testKey(1))
}

func TestIndependentCachesPublishCompleteEntries(t *testing.T) {
	dir := t.TempDir()
	caches := []*Cache{newTestCache(t, Config{Directory: dir}), newTestCache(t, Config{Directory: dir})}
	payloads := []string{strings.Repeat("a", 8192), strings.Repeat("b", 16384)}
	publish(t, caches[0], testKey(1), payloads[0])
	var wg sync.WaitGroup
	for i, c := range caches {
		wg.Add(1)
		go func(c *Cache, payload string) {
			defer wg.Done()
			for range 12 {
				w, err := c.Create(testContext, testKey(1))
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := io.WriteString(w, payload); err != nil {
					w.Close()
					t.Error(err)
					return
				}
				if err := w.Commit(Metadata{Width: 1, Height: 1, Format: "webp"}); err != nil {
					w.Close()
					t.Error(err)
					return
				}
				e, err := c.Get(testContext, testKey(1))
				if err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(e)
				e.Close()
				if err != nil || !(bytes.Equal(data, []byte(payloads[0])) || bytes.Equal(data, []byte(payloads[1]))) {
					t.Errorf("incomplete publication: %d bytes, %v", len(data), err)
					return
				}
			}
		}(c, payloads[i])
	}
	wg.Wait()
}

func TestZeroLimitsAndBackgroundErrorAccessor(t *testing.T) {
	c := newTestCache(t, Config{})
	publish(t, c, testKey(1), "data")
	setCreated(t, c, testKey(1), time.Now().Add(-365*24*time.Hour))
	result, err := c.Prune(testContext)
	if err != nil || result.Removed != 0 {
		t.Fatalf("zero limits prune: %+v %v", result, err)
	}
	e, err := c.Get(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	if err := c.LastCleanupError(); err != nil {
		t.Fatal(err)
	}
	// Drive cleanup with an invalid directory handle so error observation
	// does not depend on filesystem permissions or the test user's privileges.
	broken, err := Open(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broken.Close() })
	if err := broken.root.Close(); err != nil {
		t.Fatal(err)
	}
	broken.cfg.CleanupInterval = time.Millisecond
	broken.backgroundDone = make(chan struct{})
	go broken.cleanup()
	deadline := time.Now().Add(2 * time.Second)
	for broken.LastCleanupError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("background error was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	// Cleanup joins the goroutine; the deliberately closed root may return an error.
}

func TestCommitAndOpenSurvivesImmediateExpiryAndPrune(t *testing.T) {
	c := newTestCache(t, Config{MaxAge: time.Nanosecond, MaxBytes: 1})
	w, err := c.Create(testContext, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("published payload")); err != nil {
		t.Fatal(err)
	}
	e, err := w.CommitAndOpen(Metadata{Width: 3, Height: 2, Format: "jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Metadata.Width != 3 || e.Metadata.Height != 2 || e.Metadata.Format != "jpeg" || e.Metadata.Size != 17 || e.Metadata.Created.IsZero() {
		t.Fatalf("metadata: %+v", e.Metadata)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prune(testContext); err != nil {
		t.Fatal(err)
	}
	requireMiss(t, c, testKey(1))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(e)
	if err != nil || string(data) != "published payload" {
		t.Fatalf("commit reader: %q %v", data, err)
	}
}

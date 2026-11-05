package gimg

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"unsafe"
)

// Filter selects the reconstruction kernel used on both axes.
type Filter uint8

const (
	// Bicubic is the interpolating Catmull-Rom cubic with parameter a=-0.5.
	Bicubic Filter = iota
	// Lanczos3 is the three-lobe windowed sinc reconstruction filter.
	Lanczos3
)

var (
	// ErrClosed means Close has already released the resizer's workers.
	ErrClosed = errors.New("resize: resizer is closed")
	// ErrInvalidImage means an image is empty or its metadata/storage is invalid.
	ErrInvalidImage = errors.New("resize: invalid RGBA image")
	// ErrInvalidFilter means the filter is neither Bicubic nor Lanczos3.
	ErrInvalidFilter = errors.New("resize: invalid filter")
	// ErrOverflow means dimensions, storage, or filter arithmetic exceed bounds.
	ErrOverflow = errors.New("resize: size or coefficient overflow")
	// ErrOverlap means source and destination byte slices overlap, unless their
	// pixel mappings are exactly identical and resizing is a no-op.
	ErrOverlap = errors.New("resize: overlapping source and destination storage")
)

const maxInt = int(^uint(0) >> 1)

// RGBARowSource supplies premultiplied RGBA rows without materializing an image.
// Bounds and ScratchSize must remain constant during ResizeRows. ReadRGBARow
// receives an absolute y coordinate in Bounds and must fill the first
// 4*Bounds().Dx() bytes of dst. It may use the first ScratchSize() bytes of
// scratch, must not retain either buffer, and must support concurrent calls
// with separate buffers. Source storage must not alias the resize destination
// and must remain immutable for the duration of the call. Providers are
// responsible for validating their own storage before reading it.
type RGBARowSource interface {
	Bounds() image.Rectangle
	ScratchSize() int
	ReadRGBARow(dst, scratch []byte, y int) error
}

// Resizer owns reusable filter plans, bounded row-ring scratch, and persistent
// worker goroutines. Resize, ResizeRows, and Close are serialized, so concurrent
// calls are safe, but callers must not otherwise access image storage while it
// is in use. A Resizer must be constructed with New and must not be copied.
type Resizer struct {
	mu      sync.Mutex
	closed  bool
	x, y    axisPlan
	workers []resizeWorker
	pending sync.WaitGroup
	stopped sync.WaitGroup
}

type resizeWorker struct {
	jobs         chan struct{}
	owner        *Resizer
	src, dst     *image.RGBA
	rows         RGBARowSource
	rowMinY      int
	rgbaRow      []byte
	rowScratch   []byte
	copyRows     bool
	err          error
	first, last  int
	channels     int
	data         []int16
	rowViews     [][]int16
	rowIDs       []int
	verticalRows [][]int16
}

// New starts workers persistent goroutines. A nonpositive count selects
// runtime.GOMAXPROCS(0). Call Close when the resizer is no longer needed.
func New(workers int) *Resizer {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	r := &Resizer{workers: make([]resizeWorker, workers)}
	r.stopped.Add(workers)
	for i := range r.workers {
		w := &r.workers[i]
		w.owner = r
		w.jobs = make(chan struct{})
		go w.run()
	}
	return r
}

// Close waits for any active Resize, releases scratch and plans, and stops all
// workers. It is safe to call repeatedly. Subsequent Resize calls return ErrClosed.
func (r *Resizer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for i := range r.workers {
		close(r.workers[i].jobs)
	}
	r.stopped.Wait()
	r.workers = nil
	r.x, r.y = axisPlan{}, axisPlan{}
}

// Resize fills dst with a filtered version of src, using the images' existing
// bounds and storage. RGBA samples are premultiplied and remain in their encoded
// color space: no gamma conversion or unpremultiplication is performed. Negative
// filter lobes are retained between passes; final channels saturate to [0,255]
// and RGB is clamped to alpha. Both axes widen their filter for downsampling.
//
// Subimages, nonzero origins, and padded positive strides are supported. Empty
// images and malformed storage are rejected before native kernels run. Source
// and destination Pix ranges must not overlap, except identical pixel mappings
// of equal dimensions, which are unchanged. Equal-sized distinct images are
// copied without filtering. Caller-provided row padding is never modified.
// Repeated dimensions/filter reuse plans and worker scratch without allocation.
func (r *Resizer) Resize(dst, src *image.RGBA, filter Filter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if filter != Bicubic && filter != Lanczos3 {
		return ErrInvalidFilter
	}
	sw, sh, err := validateRGBA(src)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dw, dh, err := validateRGBA(dst)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	sameSize := sw == dw && sh == dh
	if sameSize && src.Stride == dst.Stride && unsafe.SliceData(src.Pix) == unsafe.SliceData(dst.Pix) {
		return nil
	}
	if overlapping(src.Pix, dst.Pix) {
		return ErrOverlap
	}
	if sameSize {
		for y := range sh {
			copy(dst.Pix[y*dst.Stride:][:dw*4], src.Pix[y*src.Stride:][:sw*4])
		}
		return nil
	}
	return r.resize(dst, src, nil, sw, sh, dw, dh, 0, 0, filter)
}

// ResizeRows fills dst from lazily converted RGBA rows using the same filters
// as Resize. Equal dimensions copy rows without filtering. Worker-local row
// and provider scratch are retained for allocation-free repeated calls.
// Source storage must not overlap dst; this cannot be checked through the
// interface. Provider errors are returned and may leave dst partially written.
// Resize, ResizeRows, and Close are serialized.
func (r *Resizer) ResizeRows(dst *image.RGBA, src RGBARowSource, filter Filter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if filter != Bicubic && filter != Lanczos3 {
		return ErrInvalidFilter
	}
	if src == nil {
		return fmt.Errorf("source: %w", ErrInvalidImage)
	}
	bounds := src.Bounds()
	sw, sh, err := validateRowBounds(bounds)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	scratch := src.ScratchSize()
	if scratch < 0 {
		return fmt.Errorf("source scratch: %w", ErrInvalidImage)
	}
	dw, dh, err := validateRGBA(dst)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	return r.resize(dst, nil, src, sw, sh, dw, dh, scratch, bounds.Min.Y, filter)
}

// resize schedules both source forms through the same bounded row ring.
// The caller holds r.mu and has validated source metadata and destination.
func (r *Resizer) resize(dst, src *image.RGBA, rows RGBARowSource, sw, sh, dw, dh, scratch, minY int, filter Filter) error {
	copyRows := sw == dw && sh == dh
	taps := 0
	if !copyRows {
		if err := r.x.prepare(sw, dw, filter); err != nil {
			return err
		}
		if err := r.y.prepare(sh, dh, filter); err != nil {
			return err
		}
		taps = r.y.maxTaps
		if dw*4 > maxInt/2/taps {
			return fmt.Errorf("%w: worker row ring", ErrOverflow)
		}
	}
	active := min(len(r.workers), dh)
	for i := range active {
		w := &r.workers[i]
		if !copyRows {
			w.prepare(dw*4, taps)
		}
		if rows != nil {
			w.prepareRows(sw, scratch)
		}
		w.src, w.dst, w.rows = src, dst, rows
		w.rowMinY = minY
		w.copyRows, w.err = copyRows, nil
		// Quotient/remainder partition avoids overflow in i*dh.
		band, extra := dh/active, dh%active
		w.first = i*band + min(i, extra)
		w.last = w.first + band
		if i < extra {
			w.last++
		}
	}
	r.pending.Add(active)
	for i := range active {
		r.workers[i].jobs <- struct{}{}
	}
	r.pending.Wait()
	for i := range active {
		if err := r.workers[i].err; err != nil {
			return err
		}
	}
	return nil
}

func validateRGBA(img *image.RGBA) (int, int, error) {
	if img == nil || img.Rect.Max.X <= img.Rect.Min.X || img.Rect.Max.Y <= img.Rect.Min.Y {
		return 0, 0, ErrInvalidImage
	}
	uw := uint(img.Rect.Max.X) - uint(img.Rect.Min.X)
	uh := uint(img.Rect.Max.Y) - uint(img.Rect.Min.Y)
	if uw > uint(maxInt/4) || uh > uint(maxInt) {
		return 0, 0, ErrOverflow
	}
	width, height := int(uw), int(uh)
	row := width * 4
	if img.Stride < row {
		return 0, 0, ErrInvalidImage
	}
	if height-1 > (maxInt-row)/img.Stride {
		return 0, 0, ErrOverflow
	}
	if (height-1)*img.Stride+row > len(img.Pix) {
		return 0, 0, ErrInvalidImage
	}
	return width, height, nil
}

func validateRowBounds(bounds image.Rectangle) (int, int, error) {
	if bounds.Max.X <= bounds.Min.X || bounds.Max.Y <= bounds.Min.Y {
		return 0, 0, ErrInvalidImage
	}
	width := uint(bounds.Max.X) - uint(bounds.Min.X)
	height := uint(bounds.Max.Y) - uint(bounds.Min.Y)
	if width > uint(maxInt/4) || height > uint(maxInt) {
		return 0, 0, ErrOverflow
	}
	return int(width), int(height), nil
}

func overlapping(a, b []byte) bool {
	ap, bp := uintptr(unsafe.Pointer(unsafe.SliceData(a))), uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	// Subtraction avoids overflowing an end address near the address-space limit.
	if ap <= bp {
		return bp-ap < uintptr(len(a))
	}
	return ap-bp < uintptr(len(b))
}

func (w *resizeWorker) prepare(channels, taps int) {
	size := channels * taps
	if cap(w.data) < size {
		w.data = make([]int16, size)
	} else {
		w.data = w.data[:size]
	}
	if cap(w.rowViews) < taps {
		w.rowViews = make([][]int16, taps)
		w.rowIDs = make([]int, taps)
		w.verticalRows = make([][]int16, taps)
	} else {
		w.rowViews = w.rowViews[:taps]
		w.rowIDs = w.rowIDs[:taps]
		w.verticalRows = w.verticalRows[:taps]
	}
	w.channels = channels
	for i := range taps {
		w.rowViews[i] = w.data[i*channels : (i+1)*channels]
		w.rowIDs[i] = -1
	}
}

func (w *resizeWorker) prepareRows(width, scratch int) {
	if cap(w.rgbaRow) < width*4 {
		w.rgbaRow = make([]byte, width*4)
	} else {
		w.rgbaRow = w.rgbaRow[:width*4]
	}
	if cap(w.rowScratch) < scratch {
		w.rowScratch = make([]byte, scratch)
	} else {
		w.rowScratch = w.rowScratch[:scratch]
	}
}

func (w *resizeWorker) sourceRow(y, bytes int) ([]byte, error) {
	if w.rows == nil {
		return w.src.Pix[y*w.src.Stride:][:bytes], nil
	}
	if err := w.rows.ReadRGBARow(w.rgbaRow, w.rowScratch, w.rowMinY+y); err != nil {
		return nil, err
	}
	return w.rgbaRow, nil
}

func (w *resizeWorker) run() {
	defer w.owner.stopped.Done()
	for range w.jobs {
		w.err = w.resizeBand()
		w.src, w.dst, w.rows = nil, nil, nil
		w.owner.pending.Done()
	}
}

func (w *resizeWorker) resizeBand() error {
	r := w.owner
	if w.copyRows {
		rowBytes := w.dst.Rect.Dx() * 4
		for y := w.first; y < w.last; y++ {
			source, err := w.sourceRow(y, rowBytes)
			if err != nil {
				return err
			}
			copy(w.dst.Pix[y*w.dst.Stride:][:rowBytes], source)
		}
		return nil
	}
	sourceBytes := r.x.source * 4
	taps := len(w.rowViews)
	for y := w.first; y < w.last; y++ {
		s := r.y.spans[y]
		rows := w.verticalRows[:s.count]
		for j := range s.count {
			sy := s.start + j
			slot := sy % taps
			row := w.rowViews[slot]
			if w.rowIDs[slot] != sy {
				source, err := w.sourceRow(sy, sourceBytes)
				if err != nil {
					return err
				}
				horizontalImpl(row, source, r.x.spans, r.x.coefficients)
				w.rowIDs[slot] = sy
			}
			rows[j] = row
		}
		destination := w.dst.Pix[y*w.dst.Stride:][:w.channels]
		verticalImpl(destination, rows, r.y.coefficients[s.offset:s.offset+s.count])
	}
	return nil
}

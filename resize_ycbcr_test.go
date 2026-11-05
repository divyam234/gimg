package gimg

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"sync"
	"testing"
)

// This test-only provider exercises the codec boundary without depending on a
// vendored codec extension. Actual JPEGn conversion is covered by CLI tests.
type testYCbCrRows struct {
	*image.YCbCr
}

func (s *testYCbCrRows) ScratchSize() int { return 2 * s.Rect.Dx() }

func (s *testYCbCrRows) ReadRGBARow(dst, scratch []byte, y int) error {
	width := s.Rect.Dx()
	if y < s.Rect.Min.Y || y >= s.Rect.Max.Y || len(dst) < width*4 || len(scratch) < width*2 {
		return ErrInvalidImage
	}
	for x := range width {
		i := s.COffset(s.Rect.Min.X+x, y)
		scratch[x*2], scratch[x*2+1] = s.Cb[i], s.Cr[i]
	}
	for x := range width {
		yy := s.Y[s.YOffset(s.Rect.Min.X+x, y)]
		r, g, b := color.YCbCrToRGB(yy, scratch[x*2], scratch[x*2+1])
		dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, 255
	}
	return nil
}

func patternedYCbCr(rect image.Rectangle, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	xdiv, ydiv := 1, 1
	switch ratio {
	case image.YCbCrSubsampleRatio422:
		xdiv = 2
	case image.YCbCrSubsampleRatio420:
		xdiv, ydiv = 2, 2
	case image.YCbCrSubsampleRatio440:
		ydiv = 2
	case image.YCbCrSubsampleRatio411:
		xdiv = 4
	case image.YCbCrSubsampleRatio410:
		xdiv, ydiv = 4, 2
	}
	cw := (rect.Max.X-1)/xdiv - rect.Min.X/xdiv + 1
	ch := (rect.Max.Y-1)/ydiv - rect.Min.Y/ydiv + 1
	ys, cs := rect.Dx()+7, cw+5
	src := &image.YCbCr{
		Y: make([]byte, ys*rect.Dy()), Cb: make([]byte, cs*ch), Cr: make([]byte, cs*ch),
		YStride: ys, CStride: cs, SubsampleRatio: ratio, Rect: rect,
	}
	for i := range src.Y {
		src.Y[i] = byte(i*73 + i/11*19 + 7)
	}
	for i := range src.Cb {
		src.Cb[i] = byte(i*131 + 29)
		src.Cr[i] = byte(i*47 + 211)
	}
	return src
}

func materializeYCbCr(src *image.YCbCr) *image.RGBA {
	rgba := image.NewRGBA(src.Rect)
	for y := src.Rect.Min.Y; y < src.Rect.Max.Y; y++ {
		for x := src.Rect.Min.X; x < src.Rect.Max.X; x++ {
			rgba.Set(x, y, src.At(x, y))
		}
	}
	return rgba
}

func TestResizeRowsMatchesRGBA(t *testing.T) {
	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422,
		image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio440,
		image.YCbCrSubsampleRatio411, image.YCbCrSubsampleRatio410,
	}
	for _, workers := range []int{1, 4, 32} {
		r := New(workers)
		for _, ratio := range ratios {
			for origin, rect := range []image.Rectangle{
				image.Rect(0, 0, 17, 13), image.Rect(3, 5, 20, 18),
				image.Rect(-9, -7, 8, 6), image.Rect(-7, -5, 16, 14),
			} {
				src := patternedYCbCr(rect, ratio)
				if origin == 3 {
					src = src.SubImage(image.Rect(-5, -3, 12, 10)).(*image.YCbCr)
				}
				rows, rgba := &testYCbCrRows{src}, materializeYCbCr(src)
				yBefore, cbBefore, crBefore := bytes.Clone(src.Y), bytes.Clone(src.Cb), bytes.Clone(src.Cr)
				for _, filter := range []Filter{Bicubic, Lanczos3} {
					for _, size := range [][2]int{{7, 5}, {29, 23}, {17, 13}, {17, 5}, {7, 13}, {1, 1}} {
						name := fmt.Sprintf("workers%d/ratio%d/origin%d/filter%d/%dx%d", workers, ratio, origin, filter, size[0], size[1])
						t.Run(name, func(t *testing.T) {
							stride := size[0]*4 + 13
							got := &image.RGBA{Pix: bytes.Repeat([]byte{0xa7}, stride*size[1]), Stride: stride, Rect: image.Rect(-3, 7, size[0]-3, size[1]+7)}
							want := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
							if err := r.Resize(want, rgba, filter); err != nil {
								t.Fatal(err)
							}
							if err := r.ResizeRows(got, rows, filter); err != nil {
								t.Fatal(err)
							}
							assertRGBA(t, got, want, 0)
							for y := range size[1] {
								for _, b := range got.Pix[y*got.Stride+size[0]*4 : (y+1)*got.Stride] {
									if b != 0xa7 {
										t.Fatal("destination padding modified")
									}
								}
							}
						})
					}
				}
				if !bytes.Equal(src.Y, yBefore) || !bytes.Equal(src.Cb, cbBefore) || !bytes.Equal(src.Cr, crBefore) {
					t.Fatal("row source modified")
				}
			}
		}
		r.Close()
	}
}

func TestResizeRowsNarrowAndExtremeBounds(t *testing.T) {
	r := New(3)
	defer r.Close()
	rects := []image.Rectangle{
		image.Rect(-1, -1, 0, 8), image.Rect(-1, -1, 8, 0), image.Rect(-1, -1, 0, 0),
		image.Rect(-maxInt-1, -maxInt-1, -maxInt+6, -maxInt+4),
		image.Rect(maxInt-16, maxInt-16, maxInt-9, maxInt-11),
	}
	for _, rect := range rects {
		for _, ratio := range []image.YCbCrSubsampleRatio{image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio410} {
			src := patternedYCbCr(rect, ratio)
			rgba := materializeYCbCr(src)
			for _, filter := range []Filter{Bicubic, Lanczos3} {
				got, want := image.NewRGBA(image.Rect(0, 0, 11, 7)), image.NewRGBA(image.Rect(0, 0, 11, 7))
				if err := r.Resize(want, rgba, filter); err != nil {
					t.Fatal(err)
				}
				if err := r.ResizeRows(got, &testYCbCrRows{src}, filter); err != nil {
					t.Fatal(err)
				}
				assertRGBA(t, got, want, 0)
			}
		}
	}
}

type errorRows struct {
	rect    image.Rectangle
	scratch int
	err     error
}

func (s *errorRows) Bounds() image.Rectangle              { return s.rect }
func (s *errorRows) ScratchSize() int                     { return s.scratch }
func (s *errorRows) ReadRGBARow(_, _ []byte, _ int) error { return s.err }

func TestResizeRowsValidationAndErrors(t *testing.T) {
	r := New(3)
	defer r.Close()
	sentinel := errors.New("row conversion failed")
	tests := []struct {
		name string
		src  *errorRows
		want error
	}{
		{"empty", &errorRows{rect: image.Rect(0, 0, 0, 1)}, ErrInvalidImage},
		{"inverted", &errorRows{rect: image.Rectangle{Min: image.Pt(3, 3), Max: image.Pt(1, 1)}}, ErrInvalidImage},
		{"width-overflow", &errorRows{rect: image.Rectangle{Min: image.Pt(-maxInt, 0), Max: image.Pt(maxInt, 1)}}, ErrOverflow},
		{"height-overflow", &errorRows{rect: image.Rectangle{Min: image.Pt(0, -maxInt), Max: image.Pt(1, maxInt)}}, ErrOverflow},
		{"negative-scratch", &errorRows{rect: image.Rect(0, 0, 7, 5), scratch: -1}, ErrInvalidImage},
		{"provider-error-filtered", &errorRows{rect: image.Rect(-3, -1, 14, 12), err: sentinel}, sentinel},
		{"provider-error-identity", &errorRows{rect: image.Rect(-3, -1, 2, 2), err: sentinel}, sentinel},
	}
	dst := image.NewRGBA(image.Rect(0, 0, 5, 3))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := bytes.Clone(dst.Pix)
			if err := r.ResizeRows(dst, test.src, Bicubic); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if !bytes.Equal(dst.Pix, before) {
				t.Fatal("destination modified before validation or failed row read")
			}
		})
	}
	valid := &testYCbCrRows{patternedYCbCr(image.Rect(0, 0, 9, 7), image.YCbCrSubsampleRatio420)}
	if err := r.ResizeRows(dst, nil, Bicubic); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("nil source: %v", err)
	}
	if err := r.ResizeRows(nil, valid, Bicubic); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("nil destination: %v", err)
	}
	if err := r.ResizeRows(dst, valid, Filter(255)); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid filter: %v", err)
	}
	badDst := *dst
	badDst.Stride = 1
	if err := r.ResizeRows(&badDst, valid, Bicubic); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid destination: %v", err)
	}
	// Every worker must be reusable after an error, including cross-source use.
	got, want := image.NewRGBA(dst.Rect), image.NewRGBA(dst.Rect)
	if err := r.Resize(want, materializeYCbCr(valid.YCbCr), Bicubic); err != nil {
		t.Fatal(err)
	}
	if err := r.ResizeRows(got, valid, Bicubic); err != nil {
		t.Fatal(err)
	}
	assertRGBA(t, got, want, 0)
}

func TestResizeRowsReuseAndAllocations(t *testing.T) {
	r := New(4)
	defer r.Close()
	for _, size := range [][4]int{{17, 13, 7, 5}, {31, 19, 41, 29}, {9, 7, 9, 7}, {17, 13, 7, 5}} {
		src := patternedYCbCr(image.Rect(-3, -1, size[0]-3, size[1]-1), image.YCbCrSubsampleRatio420)
		rows, rgba := &testYCbCrRows{src}, materializeYCbCr(src)
		got, want := image.NewRGBA(image.Rect(0, 0, size[2], size[3])), image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
		var callErr error
		call := func() {
			if err := r.Resize(want, rgba, Lanczos3); err != nil {
				callErr = err
			}
			if err := r.ResizeRows(got, rows, Lanczos3); err != nil {
				callErr = err
			}
		}
		call()
		if callErr != nil {
			t.Fatal(callErr)
		}
		if allocations := testing.AllocsPerRun(20, call); allocations != 0 {
			t.Fatalf("size %v: %.1f warmed allocations", size, allocations)
		}
		if callErr != nil {
			t.Fatal(callErr)
		}
		assertRGBA(t, got, want, 0)
	}
	r.Close()
	r.Close()
	if err := r.ResizeRows(nil, nil, Bicubic); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestResizeRowsConcurrentLifecycle(t *testing.T) {
	r := New(3)
	src := patternedYCbCr(image.Rect(-3, -1, 28, 22), image.YCbCrSubsampleRatio420)
	rows, rgba := &testYCbCrRows{src}, materializeYCbCr(src)
	want := image.NewRGBA(image.Rect(0, 0, 11, 9))
	if err := r.Resize(want, rgba, Bicubic); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i == 11 {
				r.Close()
				return
			}
			got := image.NewRGBA(want.Rect)
			var err error
			if i%2 == 0 {
				err = r.ResizeRows(got, rows, Bicubic)
			} else {
				err = r.Resize(got, rgba, Bicubic)
			}
			if errors.Is(err, ErrClosed) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			assertRGBA(t, got, want, 0)
		}()
	}
	close(start)
	wg.Wait()
	r.Close()
}

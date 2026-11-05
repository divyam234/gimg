package gimg

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
)

// referenceAxis deliberately evaluates every source sample rather than using
// production spans, fixed-point coefficients, or filterWeight.
func referenceAxis(source, destination, position int, filter Filter) []float64 {
	weights := make([]float64, source)
	if source == destination {
		weights[position] = 1
		return weights
	}
	scale := math.Max(1, float64(source)/float64(destination))
	center := (float64(position)+0.5)*float64(source)/float64(destination) - 0.5
	sum := 0.0
	for i := range weights {
		x := math.Abs((float64(i) - center) / scale)
		w := 0.0
		if filter == Bicubic {
			if x < 1 {
				w = 1 - 2.5*x*x + 1.5*x*x*x
			} else if x < 2 {
				w = 2 - 4*x + 2.5*x*x - 0.5*x*x*x
			}
		} else if x == 0 {
			w = 1
		} else if x < 3 {
			w = 3 * math.Sin(math.Pi*x) * math.Sin(math.Pi*x/3) / (math.Pi * math.Pi * x * x)
		}
		weights[i] = w
		sum += w
	}
	for i := range weights {
		weights[i] /= sum
	}
	return weights
}

func referenceResize(src *image.RGBA, width, height int, filter Filter) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	horizontal := make([][4]float64, width*sh)
	for x := range width {
		weights := referenceAxis(sw, width, x, filter)
		for y := range sh {
			for sx, weight := range weights {
				pixel := src.Pix[y*src.Stride+sx*4:][:4]
				for c := range 4 {
					horizontal[y*width+x][c] += float64(pixel[c]) * weight
				}
			}
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		weights := referenceAxis(sh, height, y, filter)
		for x := range width {
			var sums [4]float64
			for sy, weight := range weights {
				for c := range 4 {
					sums[c] += horizontal[sy*width+x][c] * weight
				}
			}
			pixel := dst.Pix[y*dst.Stride+x*4:][:4]
			for c := range 4 {
				pixel[c] = byte(math.Max(0, math.Min(255, math.Floor(sums[c]+0.5))))
			}
			for c := range 3 {
				if pixel[c] > pixel[3] {
					pixel[c] = pixel[3]
				}
			}
		}
	}
	return dst
}

func assertRGBA(t *testing.T, got, want *image.RGBA, tolerance int) {
	t.Helper()
	if got.Rect.Size() != want.Rect.Size() {
		t.Fatalf("size: got %v, want %v", got.Rect.Size(), want.Rect.Size())
	}
	for y := range want.Rect.Dy() {
		for x := range want.Rect.Dx() {
			for c := range 4 {
				a, b := int(got.Pix[y*got.Stride+x*4+c]), int(want.Pix[y*want.Stride+x*4+c])
				if d := a - b; d < -tolerance || d > tolerance {
					t.Fatalf("pixel (%d,%d) channel %d: got %d, want %d (tolerance %d)", x, y, c, a, b, tolerance)
				}
			}
		}
	}
}

func patternedRGBA(width, height int, alpha bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			a := 255
			if alpha {
				a = (x*37 + y*61) % 256
			}
			img.SetRGBA(x, y, color.RGBA{uint8((x*79 + y*23) % (a + 1)), uint8((x*17 + y*97) % (a + 1)), uint8((x*43 + y*11) % (a + 1)), uint8(a)})
		}
	}
	return img
}

func TestResizeFloatReference(t *testing.T) {
	r := New(3)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, size := range [][4]int{{7, 5, 19, 13}, {31, 23, 8, 6}, {9, 7, 9, 19}, {9, 7, 3, 7}, {1, 9, 13, 4}, {9, 1, 4, 13}, {17, 15, 1, 1}} {
			for _, alpha := range []bool{false, true} {
				src := patternedRGBA(size[0], size[1], alpha)
				dst := image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
				if err := r.Resize(dst, src, filter); err != nil {
					t.Fatalf("filter %d, size %v: %v", filter, size, err)
				}
				assertRGBA(t, dst, referenceResize(src, size[2], size[3], filter), 2)
				for y := range size[3] {
					for x := range size[2] {
						p := dst.RGBAAt(x, y)
						if p.R > p.A || p.G > p.A || p.B > p.A {
							t.Fatalf("invalid premultiplied pixel: %v", p)
						}
						if !alpha && p.A != 255 {
							t.Fatalf("opaque input lost opacity: %v", p)
						}
					}
				}
			}
		}
	}
}

func TestResizeCenteredImpulse(t *testing.T) {
	r := New(2)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, dimensions := range [][2]int{{5, 17}, {17, 5}, {6, 13}} {
			n, m := dimensions[0], dimensions[1]
			src := image.NewRGBA(image.Rect(0, 0, n, 1))
			for x := range n {
				src.SetRGBA(x, 0, color.RGBA{A: 255})
			}
			src.SetRGBA(n/2, 0, color.RGBA{255, 255, 255, 255})
			dst := image.NewRGBA(image.Rect(0, 0, m, 1))
			if err := r.Resize(dst, src, filter); err != nil {
				t.Fatal(err)
			}
			assertRGBA(t, dst, referenceResize(src, m, 1, filter), 2)
			if n%2 == 1 {
				for x := range m {
					delta := int(dst.Pix[x*4]) - int(dst.Pix[(m-1-x)*4])
					if delta < -1 || delta > 1 {
						t.Fatalf("off-center impulse: %v", dst.Pix)
					}
				}
			}
		}
	}
}

func TestResizeConstants(t *testing.T) {
	r := New(4)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, value := range []color.RGBA{{0, 0, 0, 0}, {31, 70, 123, 127}, {255, 255, 255, 255}} {
			for _, size := range [][4]int{{3, 5, 19, 17}, {31, 27, 4, 3}, {7, 5, 7, 19}, {7, 19, 3, 19}, {1, 1, 9, 7}, {13, 9, 1, 1}} {
				src := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
				for y := range size[1] {
					for x := range size[0] {
						src.SetRGBA(x, y, value)
					}
				}
				dst := image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
				if err := r.Resize(dst, src, filter); err != nil {
					t.Fatal(err)
				}
				for y := range size[3] {
					for x := range size[2] {
						if got := dst.RGBAAt(x, y); got != value {
							t.Fatalf("filter %d size %v: got %v, want %v", filter, size, got, value)
						}
					}
				}
			}
		}
	}
}

func TestResizeReductionAntialiasing(t *testing.T) {
	r := New(2)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, size := range [][4]int{{63, 47, 7, 5}, {63, 9, 7, 9}, {9, 63, 9, 7}} {
			src := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
			for y := range size[1] {
				for x := range size[0] {
					v := uint8(0)
					phase := x + y
					if size[0] == size[2] {
						phase = y
					} else if size[1] == size[3] {
						phase = x
					}
					if phase%2 != 0 {
						v = 255
					}
					src.SetRGBA(x, y, color.RGBA{v, v, v, 255})
				}
			}
			dst := image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
			if err := r.Resize(dst, src, filter); err != nil {
				t.Fatal(err)
			}
			assertRGBA(t, dst, referenceResize(src, size[2], size[3], filter), 2)
			for y := range size[3] {
				for x := range size[2] {
					if v := dst.RGBAAt(x, y).R; v < 112 || v > 143 {
						t.Fatalf("aliased reduction, filter %d size %v at (%d,%d): %d", filter, size, x, y, v)
					}
				}
			}
		}
	}
}

func TestResizePremultipliedFringes(t *testing.T) {
	r := New(2)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		src := image.NewRGBA(image.Rect(0, 0, 13, 7))
		for y := 1; y < 6; y++ {
			for x := 4; x < 9; x++ {
				src.SetRGBA(x, y, color.RGBA{128, 0, 0, 128})
			}
		}
		for _, size := range [][2]int{{41, 23}, {5, 3}} {
			dst := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
			if err := r.Resize(dst, src, filter); err != nil {
				t.Fatal(err)
			}
			assertRGBA(t, dst, referenceResize(src, size[0], size[1], filter), 2)
			for y := range size[1] {
				for x := range size[0] {
					p := dst.RGBAAt(x, y)
					if p.R != p.A || p.G != 0 || p.B != 0 {
						t.Fatalf("premultiplied red acquired fringe at (%d,%d): %v", x, y, p)
					}
				}
			}
		}
	}
}

func TestResizeSubimagesAndPadding(t *testing.T) {
	r := New(3)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, size := range [][2]int{{11, 9}, {5, 4}} {
			sourceParent := image.NewRGBA(image.Rect(-7, -5, 9, 11))
			src := sourceParent.SubImage(image.Rect(-3, -2, 2, 2)).(*image.RGBA)
			pattern := patternedRGBA(5, 4, true)
			for y := range 4 {
				copy(src.Pix[y*src.Stride:][:20], pattern.Pix[y*pattern.Stride:][:20])
			}
			beforeSource := bytes.Clone(sourceParent.Pix)
			parent := image.NewRGBA(image.Rect(10, 20, 10+size[0]+7, 20+size[1]+6))
			for i := range parent.Pix {
				parent.Pix[i] = 0xa7
			}
			dst := parent.SubImage(image.Rect(13, 22, 13+size[0], 22+size[1])).(*image.RGBA)
			if err := r.Resize(dst, src, filter); err != nil {
				t.Fatal(err)
			}
			assertRGBA(t, dst, referenceResize(pattern, size[0], size[1], filter), 2)
			if !bytes.Equal(sourceParent.Pix, beforeSource) {
				t.Fatal("source modified")
			}
			for y := parent.Rect.Min.Y; y < parent.Rect.Max.Y; y++ {
				for x := parent.Rect.Min.X; x < parent.Rect.Max.X; x++ {
					if image.Pt(x, y).In(dst.Rect) {
						continue
					}
					if p := parent.RGBAAt(x, y); p != (color.RGBA{0xa7, 0xa7, 0xa7, 0xa7}) {
						t.Fatalf("padding changed at (%d,%d): %v", x, y, p)
					}
				}
			}
		}
	}
}

func TestResizeRejectsMalformedStorage(t *testing.T) {
	r := New(1)
	defer r.Close()
	valid := patternedRGBA(3, 2, false)
	cases := []struct {
		name string
		img  *image.RGBA
		want error
	}{
		{"nil", nil, ErrInvalidImage},
		{"empty", &image.RGBA{}, ErrInvalidImage},
		{"reversed", &image.RGBA{Rect: image.Rectangle{Min: image.Pt(2, 0), Max: image.Pt(1, 1)}}, ErrInvalidImage},
		{"short stride", &image.RGBA{Pix: make([]byte, 16), Stride: 3, Rect: image.Rect(0, 0, 2, 2)}, ErrInvalidImage},
		{"negative stride", &image.RGBA{Pix: make([]byte, 16), Stride: -8, Rect: image.Rect(0, 0, 2, 2)}, ErrInvalidImage},
		{"short storage", &image.RGBA{Pix: make([]byte, 15), Stride: 8, Rect: image.Rect(0, 0, 2, 2)}, ErrInvalidImage},
		{"width overflow", &image.RGBA{Rect: image.Rectangle{Min: image.Pt(-maxInt, 0), Max: image.Pt(maxInt, 1)}}, ErrOverflow},
		{"height overflow", &image.RGBA{Rect: image.Rectangle{Min: image.Pt(0, -maxInt), Max: image.Pt(1, maxInt)}}, ErrOverflow},
		{"row overflow", &image.RGBA{Stride: maxInt, Rect: image.Rect(0, 0, 1, 3)}, ErrOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Resize(valid, tc.img, Bicubic); !errors.Is(err, tc.want) {
				t.Fatalf("source error: got %v, want %v", err, tc.want)
			}
			if err := r.Resize(tc.img, valid, Lanczos3); !errors.Is(err, tc.want) {
				t.Fatalf("destination error: got %v, want %v", err, tc.want)
			}
		})
	}
	if err := r.Resize(valid, valid, Filter(255)); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid filter: %v", err)
	}
}

func TestResizeOverlapAndIdentity(t *testing.T) {
	r := New(1)
	defer r.Close()
	src := patternedRGBA(5, 4, true)
	before := bytes.Clone(src.Pix)
	if err := r.Resize(src, src, Lanczos3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src.Pix, before) {
		t.Fatal("identity modified pixels")
	}
	alias := *src
	alias.Rect = alias.Rect.Add(image.Pt(27, -19))
	if err := r.Resize(&alias, src, Bicubic); err != nil {
		t.Fatalf("identical pixel mapping: %v", err)
	}
	for _, bounds := range []image.Rectangle{image.Rect(1, 0, 5, 4), image.Rect(0, 1, 5, 4)} {
		dst := src.SubImage(bounds).(*image.RGBA)
		if err := r.Resize(dst, src, Bicubic); !errors.Is(err, ErrOverlap) {
			t.Fatalf("overlap: %v", err)
		}
		if err := r.Resize(src, dst, Lanczos3); !errors.Is(err, ErrOverlap) {
			t.Fatalf("reverse overlap: %v", err)
		}
	}
	backing := make([]byte, 24)
	a := &image.RGBA{Pix: backing[:16], Stride: 8, Rect: image.Rect(0, 0, 2, 2)}
	b := &image.RGBA{Pix: backing[4:20], Stride: 8, Rect: image.Rect(0, 0, 2, 2)}
	if err := r.Resize(b, a, Bicubic); !errors.Is(err, ErrOverlap) {
		t.Fatalf("equal-size shifted overlap: %v", err)
	}
	if !bytes.Equal(src.Pix, before) {
		t.Fatal("rejected overlap modified pixels")
	}
	dst := image.NewRGBA(src.Rect)
	if err := r.Resize(dst, src, Lanczos3); err != nil {
		t.Fatal(err)
	}
	assertRGBA(t, dst, src, 0)
}

func TestResizeReuseAndClose(t *testing.T) {
	r := New(3)
	defer r.Close()
	for repeat := range 3 {
		for _, filter := range []Filter{Lanczos3, Bicubic, Lanczos3} {
			for _, size := range [][4]int{{19, 17, 7, 5}, {4, 3, 13, 11}, {19, 17, 7, 5}, {7, 5, 7, 5}, {19, 17, 7, 5}} {
				src := patternedRGBA(size[0], size[1], repeat%2 != 0)
				dst := image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
				if err := r.Resize(dst, src, filter); err != nil {
					t.Fatal(err)
				}
				assertRGBA(t, dst, referenceResize(src, size[2], size[3], filter), 2)
			}
		}
	}
	r.Close()
	r.Close()
	if err := r.Resize(nil, nil, Bicubic); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed resize: %v", err)
	}
}

func TestResizePlanFailureDoesNotPoisonCache(t *testing.T) {
	r := New(1)
	defer r.Close()
	src := patternedRGBA(11, 7, false)
	dst := image.NewRGBA(image.Rect(0, 0, 5, 3))
	if err := r.Resize(dst, src, Bicubic); err != nil {
		t.Fatal(err)
	}
	// This fails before allocating coefficient storage, after span storage has
	// been repurposed. Returning to the previous cache key must rebuild it.
	if err := r.x.prepare(maxInt/4, 3, Lanczos3); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected coefficient overflow, got %v", err)
	}
	if err := r.Resize(dst, src, Bicubic); err != nil {
		t.Fatal(err)
	}
	assertRGBA(t, dst, referenceResize(src, 5, 3, Bicubic), 2)
	if err := r.y.prepare(maxInt/4, 3, Lanczos3); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected coefficient overflow, got %v", err)
	}
	if err := r.Resize(dst, src, Bicubic); err != nil {
		t.Fatal(err)
	}
	assertRGBA(t, dst, referenceResize(src, 5, 3, Bicubic), 2)
}

func TestResizeWarmCallsDoNotAllocate(t *testing.T) {
	r := New(2)
	defer r.Close()
	src := patternedRGBA(31, 23, true)
	dst := image.NewRGBA(image.Rect(0, 0, 11, 9))
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		if err := r.Resize(dst, src, filter); err != nil {
			t.Fatal(err)
		}
		var resizeErr error
		allocations := testing.AllocsPerRun(20, func() { resizeErr = r.Resize(dst, src, filter) })
		if resizeErr != nil {
			t.Fatal(resizeErr)
		}
		if allocations != 0 {
			t.Fatalf("warmed filter %d: %g allocations per call", filter, allocations)
		}
	}
}

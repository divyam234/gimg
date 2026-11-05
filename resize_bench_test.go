package gimg

import (
	"fmt"
	"image"
	"testing"
)

// BenchmarkResize measures warmed resamplers over a 16-megapixel RGBA image.
// Decode, encode, source generation, and caller-owned output allocation are
// excluded; the complete Resize call, including worker synchronization, is timed.
func BenchmarkResize(b *testing.B) {
	const width, height = 4928, 3279
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		row := src.Pix[y*src.Stride : (y+1)*src.Stride]
		for x := range width {
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = byte(x+y), byte(3*x+y), byte(x+5*y), 255
		}
	}
	for _, filter := range []struct {
		name string
		kind Filter
	}{{"bicubic", Bicubic}, {"lanczos3", Lanczos3}} {
		for _, size := range []image.Point{{X: 320, Y: 213}, {X: 1280, Y: 852}} {
			for _, workers := range []int{1, 4} {
				name := fmt.Sprintf("%s/%dx%d/workers%d", filter.name, size.X, size.Y, workers)
				b.Run(name, func(b *testing.B) {
					r := New(workers)
					defer r.Close()
					dst := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
					if err := r.Resize(dst, src, filter.kind); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(int64(width * height * 4))
					for b.Loop() {
						if err := r.Resize(dst, src, filter.kind); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

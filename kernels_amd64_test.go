//go:build amd64 && !purego

package gimg

import (
	"slices"
	"testing"

	"golang.org/x/sys/cpu"
)

// Keep coefficients normalized, include negative lobes, and stay within the
// planner's absolute-sum bound. In particular, two taps exercise a weight
// larger than 16384 without overflowing int16.
func avx2TestCoefficients(n int) []int16 {
	weights := make([]int16, n)
	base := 16384 / n
	for i := range weights {
		weights[i] = int16(base)
	}
	if n > 1 {
		weights[0] = int16(-base / 2)
	}
	sum := 0
	for _, w := range weights {
		sum += int(w)
	}
	weights[n-1] += int16(16384 - sum)
	return weights
}

func TestAVX2HorizontalParity(t *testing.T) {
	if !cpu.X86.HasAVX2 {
		t.Skip("AVX2 unavailable")
	}
	for _, alignment := range []int{0, 1, 3, 7, 15, 31} {
		for _, pixels := range []int{0, 1, 2, 3, 7, 16, 33} {
			const sourcePixels = 80
			storage := make([]byte, alignment+sourcePixels*4)
			src := storage[alignment:]
			for p := 0; p < sourcePixels; p++ {
				a := byte((p*47 + alignment*13) & 255)
				src[p*4] = byte((p*71 + 9) % (int(a) + 1))
				src[p*4+1] = byte((p*37 + 3) % (int(a) + 1))
				src[p*4+2] = byte((p*13 + 7) % (int(a) + 1))
				src[p*4+3] = a
			}
			for taps := 1; taps <= 33; taps++ {
				spans := make([]span, pixels)
				// Offset coefficients by one int16 to exercise unaligned loads.
				coefficientStorage := []int16{12345}
				for x := range spans {
					n := 1 + (taps+x)%33
					spans[x] = span{start: (x * 7) % (sourcePixels - n + 1), offset: len(coefficientStorage) - 1, count: n}
					coefficientStorage = append(coefficientStorage, avx2TestCoefficients(n)...)
				}
				coefficients := coefficientStorage[1:]
				want := make([]int16, pixels*4)
				horizontalScalar(want, src, spans, coefficients)
				guarded := make([]int16, pixels*4+2)
				guarded[0], guarded[len(guarded)-1] = 1234, -4321
				got := guarded[1 : len(guarded)-1]
				horizontalAVX2(got, src, spans, coefficients)
				if !slices.Equal(got, want) {
					t.Fatalf("alignment=%d pixels=%d taps=%d: got %v, want %v", alignment, pixels, taps, got, want)
				}
				if guarded[0] != 1234 || guarded[len(guarded)-1] != -4321 {
					t.Fatalf("horizontal overwrote guard: alignment=%d pixels=%d taps=%d", alignment, pixels, taps)
				}
			}
		}
	}
}

func TestAVX2HorizontalWideAccumulatorParity(t *testing.T) {
	if !cpu.X86.HasAVX2 {
		t.Skip("AVX2 unavailable")
	}
	for _, taps := range []int{4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 128, 129, 257} {
		// Put positive weights in the low packet lane and negative weights in
		// the high lane. The lanes only cancel after the full packet loop.
		// The normalized weights reach the planner's absolute-sum limit.
		positive := 0
		for i := range taps {
			if i%4 < 2 {
				positive++
			}
		}
		negative := taps - positive
		for _, alignment := range []int{0, 1, 3, 7, 15, 31} {
			storage := make([]byte, alignment+taps*4)
			src := storage[alignment:]
			coefficientStorage := make([]int16, taps+1)
			coefficients := coefficientStorage[1:]
			positiveLeft, negativeLeft := 24576, 8192
			positiveCount, negativeCount := positive, negative
			for i := range coefficients {
				if i%4 < 2 {
					weight := positiveLeft / positiveCount
					coefficients[i] = int16(weight)
					positiveLeft -= weight
					positiveCount--
					src[i*4] = 255
				} else {
					weight := negativeLeft / negativeCount
					coefficients[i] = int16(-weight)
					negativeLeft -= weight
					negativeCount--
					src[i*4+1] = 255
				}
				src[i*4+2] = byte((i*71 + alignment*13) & 255)
				src[i*4+3] = 255
			}
			// Both source and coefficient slices end exactly at the final tap.
			spans := []span{{count: taps}}
			want := make([]int16, 4)
			horizontalScalar(want, src, spans, coefficients)
			guarded := []int16{1234, 0, 0, 0, 0, -4321}
			got := guarded[1:5]
			horizontalAVX2(got, src, spans, coefficients)
			if !slices.Equal(got, want) {
				t.Fatalf("taps=%d alignment=%d: got %v, want %v", taps, alignment, got, want)
			}
			if guarded[0] != 1234 || guarded[5] != -4321 {
				t.Fatalf("horizontal overwrote guard: taps=%d alignment=%d", taps, alignment)
			}
		}
	}
}

func TestAVX2VerticalParity(t *testing.T) {
	if !cpu.X86.HasAVX2 {
		t.Skip("AVX2 unavailable")
	}
	for _, pixels := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 17, 32, 33} {
		for _, taps := range []int{1, 2, 3, 4, 7, 16, 33} {
			for _, offset := range []int{0, 1, 3, 7} {
				rows := make([][]int16, taps)
				for y := range rows {
					padding := offset + y%3
					storage := make([]int16, padding+pixels*4)
					rows[y] = storage[padding:]
					for c := range rows[y] {
						// Signed intermediate values and independent alpha force
						// negative/positive saturation and RGB-to-alpha clamping.
						rows[y][c] = int16((y*7919+c*3571+offset*17)%65536 - 32768)
					}
				}
				coefficientStorage := append([]int16{999}, avx2TestCoefficients(taps)...)
				coefficients := coefficientStorage[1:]
				want := make([]byte, pixels*4)
				verticalScalar(want, rows, coefficients)
				guarded := make([]byte, offset+pixels*4+1)
				for i := range guarded {
					guarded[i] = 0xa5
				}
				got := guarded[offset : offset+pixels*4]
				verticalAVX2(got, rows, coefficients)
				if !slices.Equal(got, want) {
					t.Fatalf("pixels=%d taps=%d offset=%d: got %v, want %v", pixels, taps, offset, got, want)
				}
				for _, b := range guarded[:offset] {
					if b != 0xa5 {
						t.Fatal("vertical overwrote leading guard")
					}
				}
				if guarded[len(guarded)-1] != 0xa5 {
					t.Fatal("vertical overwrote trailing guard")
				}
				for p := 0; p < pixels; p++ {
					if got[p*4] > got[p*4+3] || got[p*4+1] > got[p*4+3] || got[p*4+2] > got[p*4+3] {
						t.Fatalf("non-premultiplied output at pixel %d: %v", p, got[p*4:p*4+4])
					}
				}
			}
		}
	}
}

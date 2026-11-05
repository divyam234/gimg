//go:build arm64 && !purego

package gimg

import (
	"slices"
	"testing"
)

// These tests execute the native backend directly rather than relying on init
// dispatch. Run on ARM64 hardware; cross-compilation alone cannot check parity.
func neonTestWeights(n int) []int16 {
	weights := make([]int16, n)
	for i := range weights {
		weights[i] = int16(16384 / n)
		if i < 16384%n {
			weights[i]++
		}
	}
	if n > 1 {
		weights[0] -= 4096
		weights[1] += 4096
	}
	return weights
}

func TestNEONHorizontalParity(t *testing.T) {
	for _, taps := range []int{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 33} {
		weights := neonTestWeights(taps)
		for width := 1; width <= 19; width++ {
			// The last span ends exactly at the end of src: tap tails must
			// not depend on padding after either source or coefficients.
			src := make([]byte, (taps+width-1)*4)
			for x := 0; x < len(src)/4; x++ {
				a := byte((x*73 + taps*31) & 255)
				src[x*4+0] = byte((x * 29) % (int(a) + 1))
				src[x*4+1] = byte((x*61 + 7) % (int(a) + 1))
				src[x*4+2] = byte((x*19 + 3) % (int(a) + 1))
				src[x*4+3] = a
			}
			spans := make([]span, width)
			coefficients := make([]int16, 0, width*taps)
			for x := range spans {
				spans[x] = span{start: x, offset: len(coefficients), count: taps}
				coefficients = append(coefficients, weights...)
			}
			want := make([]int16, width*4)
			horizontalScalar(want, src, spans, coefficients)
			guarded := make([]int16, width*4+2)
			guarded[0], guarded[len(guarded)-1] = -23456, 23456
			got := guarded[1 : len(guarded)-1]
			horizontalNEON(got, src, spans, coefficients)
			if !slices.Equal(got, want) {
				t.Fatalf("width=%d taps=%d: got %v, want %v", width, taps, got, want)
			}
			if guarded[0] != -23456 || guarded[len(guarded)-1] != 23456 {
				t.Fatalf("width=%d taps=%d: horizontal destination overrun", width, taps)
			}
		}
	}
	horizontalNEON(nil, nil, nil, nil)
}

func TestNEONHorizontalSignedRounding(t *testing.T) {
	for _, tc := range []struct {
		weight int16
		pixel  byte
		want   int16
	}{
		{256, 1, 1},
		{-256, 1, 0},
		{-256, 3, -1},
		{-2048, 255, -1020},
	} {
		src := []byte{tc.pixel, tc.pixel, tc.pixel, tc.pixel, 0, 0, 0, 0}
		weights := []int16{tc.weight, 16384 - tc.weight}
		got := make([]int16, 4)
		horizontalNEON(got, src, []span{{count: 2}}, weights)
		for c, value := range got {
			if value != tc.want {
				t.Fatalf("weight=%d pixel=%d channel=%d: got %d, want %d", tc.weight, tc.pixel, c, value, tc.want)
			}
		}
	}
}

func TestNEONVerticalParity(t *testing.T) {
	for _, taps := range []int{1, 2, 3, 4, 5, 7, 8, 9, 16, 17, 33} {
		weights := neonTestWeights(taps)
		for width := 1; width <= 19; width++ {
			rows := make([][]int16, taps)
			for y := range rows {
				rows[y] = make([]int16, width*4)
				for c := range rows[y] {
					// Include signed ringing, out-of-byte-range values and
					// independent alpha to exercise final premultiplication.
					rows[y][c] = int16((y*4799+c*2027+width*103)%24576 - 8192)
				}
			}
			want := make([]byte, width*4)
			verticalScalar(want, rows, weights)
			guarded := make([]byte, width*4+2)
			guarded[0], guarded[len(guarded)-1] = 123, 231
			got := guarded[1 : len(guarded)-1]
			verticalNEON(got, rows, weights)
			if !slices.Equal(got, want) {
				t.Fatalf("width=%d taps=%d: got %v, want %v", width, taps, got, want)
			}
			if guarded[0] != 123 || guarded[len(guarded)-1] != 231 {
				t.Fatalf("width=%d taps=%d: vertical destination overrun", width, taps)
			}
			for x := 0; x < len(got); x += 4 {
				if got[x] > got[x+3] || got[x+1] > got[x+3] || got[x+2] > got[x+3] {
					t.Fatalf("width=%d taps=%d: invalid premultiplied pixel %v", width, taps, got[x:x+4])
				}
			}
		}
	}
	verticalNEON(nil, nil, nil)
}

func TestNEONVerticalRoundingAndAlpha(t *testing.T) {
	// Four-pixel packets plus a scalar-tail pixel, with half-way signed
	// values and byte saturation. Deliberately over-alpha RGB must clamp.
	row := []int16{
		-17, -16, -15, 8160,
		15, 16, 17, 8160,
		16320, 8160, 4096, 32,
		-8192, 0, 32, 0,
		16, 48, 80, 112,
	}
	want := []byte{
		0, 0, 0, 255,
		0, 1, 1, 255,
		1, 1, 1, 1,
		0, 0, 0, 0,
		1, 2, 3, 4,
	}
	got := make([]byte, len(row))
	verticalNEON(got, [][]int16{row}, []int16{16384})
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

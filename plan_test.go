package gimg

import (
	"image"
	"math"
	"testing"
)

// untrimmedFixedAxis reconstructs the original full support independently of
// axisPlan. Keep zero coefficients: these are the work the optimized plan omits.
func untrimmedFixedAxis(source, destination, position int, filter Filter) (int, []int16) {
	if source == destination {
		return position, []int16{1 << coefficientBits}
	}
	ratio := float64(source) / float64(destination)
	scale := math.Max(1, ratio)
	center := (float64(position)+0.5)*ratio - 0.5
	radius := 2.0
	if filter == Lanczos3 {
		radius = 3
	}
	start := int(math.Max(0, math.Ceil(center-radius*scale)))
	end := int(math.Min(float64(source-1), math.Floor(center+radius*scale)))
	weights := make([]int16, end-start+1)
	if len(weights) == 1 {
		weights[0] = 1 << coefficientBits
		return start, weights
	}
	// Match the original floating-point evaluation order, not an algebraically
	// equivalent kernel: rounding at half-integer coefficients must be exact.
	weightAt := func(sample int) float64 {
		x := math.Abs((float64(sample) - center) / scale)
		if filter == Bicubic {
			if x < 1 {
				return ((1.5*x-2.5)*x)*x + 1
			}
			if x < 2 {
				return ((-0.5*x+2.5)*x-4)*x + 2
			}
			return 0
		}
		if x == 0 {
			return 1
		}
		if x >= 3 {
			return 0
		}
		p := math.Pi * x
		return (math.Sin(p) / p) * (math.Sin(p/3) / (p / 3))
	}
	sum := 0.0
	for sample := start; sample <= end; sample++ {
		sum += weightAt(sample)
	}
	fixedSum, largest, largestWeight := 0, 0, math.Inf(-1)
	for j := range weights {
		weight := weightAt(start+j) / sum
		if weight > largestWeight {
			largest, largestWeight = j, weight
		}
		fixed := int(math.Round(weight * (1 << coefficientBits)))
		weights[j] = int16(fixed)
		fixedSum += fixed
	}
	weights[largest] = int16(int(weights[largest]) + (1 << coefficientBits) - fixedSum)
	return start, weights
}

func TestAxisPlanTrimsOnlyFixedpointZeroBoundaries(t *testing.T) {
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		trimmed, smallerRing := false, false
		check := func(source, destination int) {
			t.Helper()
			var plan axisPlan
			if err := plan.prepare(source, destination, filter); err != nil {
				t.Fatalf("filter %d %d -> %d: %v", filter, source, destination, err)
			}
			maxTaps, untrimmedMax, offset := 0, 0, 0
			for x, s := range plan.spans {
				start, weights := untrimmedFixedAxis(source, destination, x, filter)
				untrimmedMax = max(untrimmedMax, len(weights))
				first, last := 0, len(weights)
				for weights[first] == 0 {
					first++
				}
				for weights[last-1] == 0 {
					last--
				}
				if s.start != start+first || s.offset != offset+first || s.count != last-first {
					t.Fatalf("filter %d %d -> %d phase %d: span %+v, want start %d offset %d count %d", filter, source, destination, x, s, start+first, offset+first, last-first)
				}
				sum := 0
				for j, want := range weights[first:last] {
					got := plan.coefficients[s.offset+j]
					if got != want {
						t.Fatalf("filter %d %d -> %d phase %d tap %d: got %d want %d", filter, source, destination, x, j, got, want)
					}
					sum += int(got)
				}
				if sum != 1<<coefficientBits {
					t.Fatalf("coefficient sum: %d", sum)
				}
				trimmed = trimmed || s.count < len(weights)
				maxTaps = max(maxTaps, s.count)
				offset += len(weights)
			}
			if plan.maxTaps != maxTaps {
				t.Fatalf("maxTaps: got %d want %d", plan.maxTaps, maxTaps)
			}
			smallerRing = smallerRing || maxTaps < untrimmedMax
		}
		// Exhaustive small ratios cover integer-aligned and fractional phases,
		// clipped boundaries, identity, single-sample axes, and both directions.
		for source := 1; source <= 40; source++ {
			for destination := 1; destination <= 40; destination++ {
				check(source, destination)
			}
		}
		for _, dimensions := range [][2]int{{4096, 3}, {3, 4096}, {8191, 1}, {1, 8191}, {4928, 1280}} {
			check(dimensions[0], dimensions[1])
		}
		if !trimmed || !smallerRing {
			t.Fatalf("filter %d: no reduced spans (%v) or ring capacity (%v)", filter, trimmed, smallerRing)
		}
	}
}

// untrimmedFixedResize uses full intermediate rows and independent scalar
// convolution, without production spans, row rings, or horizontal/verticalImpl.
func untrimmedFixedResize(src *image.RGBA, width, height int, filter Filter) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	rows := make([]int16, sh*width*4)
	for x := range width {
		start, weights := untrimmedFixedAxis(sw, width, x, filter)
		for y := range sh {
			for c := range 4 {
				var sum int32
				for j, weight := range weights {
					sum += int32(src.Pix[y*src.Stride+(start+j)*4+c]) * int32(weight)
				}
				rows[(y*width+x)*4+c] = int16((sum + (1 << (horizontalShift - 1))) >> horizontalShift)
			}
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		start, weights := untrimmedFixedAxis(sh, height, y, filter)
		for x := range width {
			pixel := dst.Pix[y*dst.Stride+x*4:][:4]
			for c := range 4 {
				var sum int32
				for j, weight := range weights {
					sum += int32(rows[((start+j)*width+x)*4+c]) * int32(weight)
				}
				value := (sum + (1 << (verticalShift - 1))) >> verticalShift
				pixel[c] = byte(max(0, min(255, value)))
			}
			for c := range 3 {
				pixel[c] = min(pixel[c], pixel[3])
			}
		}
	}
	return dst
}

func TestResizeUntrimmedFixedpointParity(t *testing.T) {
	r := New(3)
	defer r.Close()
	for _, filter := range []Filter{Bicubic, Lanczos3} {
		for _, size := range [][4]int{
			{7, 5, 19, 13}, {31, 23, 8, 6}, {9, 7, 9, 19}, {9, 7, 3, 7},
			{1, 9, 13, 4}, {9, 1, 4, 13}, {17, 15, 1, 1}, {1, 1, 1, 1},
			{11, 13, 11, 13}, {33, 35, 11, 7}, {2048, 7, 3, 19}, {3, 19, 2048, 7},
			{7, 2048, 19, 3}, {19, 3, 7, 2048},
		} {
			for _, alpha := range []bool{false, true} {
				src := patternedRGBA(size[0], size[1], alpha)
				dst := image.NewRGBA(image.Rect(0, 0, size[2], size[3]))
				if err := r.Resize(dst, src, filter); err != nil {
					t.Fatalf("filter %d size %v alpha %v: %v", filter, size, alpha, err)
				}
				assertRGBA(t, dst, untrimmedFixedResize(src, size[2], size[3], filter), 0)
			}
		}
	}
}

package gimg

import (
	"fmt"
	"math"
)

type axisPlan struct {
	source, destination int
	filter              Filter
	spans               []span
	coefficients        []int16
	maxTaps             int
}

func (p *axisPlan) prepare(source, destination int, filter Filter) error {
	if p.source == source && p.destination == destination && p.filter == filter {
		return nil
	}
	// Preparing reuses and mutates backing storage. Invalidate the old cache
	// key first so a failed preparation cannot expose a partially rebuilt plan
	// as the previous successful one.
	p.source, p.destination = 0, 0
	if cap(p.spans) < destination {
		p.spans = make([]span, destination)
	} else {
		p.spans = p.spans[:destination]
	}
	ratio := float64(source) / float64(destination)
	scale := math.Max(1, ratio)
	radius := 2.0
	if filter == Lanczos3 {
		radius = 3
	}
	support := radius * scale
	total := 0
	for x := range p.spans {
		start, count := 0, 1
		if source == destination {
			start = x
		} else {
			center := (float64(x)+0.5)*ratio - 0.5
			// Clip in floating point before converting: a widened support can
			// extend beyond the representable integer range.
			lo := math.Max(0, math.Ceil(center-support))
			hi := math.Min(float64(source-1), math.Floor(center+support))
			start = int(lo)
			if start >= source {
				start = source - 1
			}
			end := int(hi)
			if end >= source {
				end = source - 1
			}
			count = end - start + 1
		}
		if count <= 0 || total > maxInt-count {
			return fmt.Errorf("%w: coefficient count", ErrOverflow)
		}
		p.spans[x] = span{start: start, offset: total, count: count}
		total += count
	}
	if total > maxInt/2 {
		return fmt.Errorf("%w: coefficient storage", ErrOverflow)
	}
	if cap(p.coefficients) < total {
		p.coefficients = make([]int16, total)
	} else {
		p.coefficients = p.coefficients[:total]
	}
	for x, s := range p.spans {
		weights := p.coefficients[s.offset : s.offset+s.count]
		if source == destination || s.count == 1 {
			weights[0] = 1 << coefficientBits
			continue
		}
		center := (float64(x)+0.5)*ratio - 0.5
		sum := 0.0
		for j := 0; j < s.count; j++ {
			sum += filterWeight((float64(s.start+j)-center)/scale, filter)
		}
		if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
			return fmt.Errorf("%w: degenerate filter weights", ErrInvalidImage)
		}
		fixedSum, largest, largestWeight := 0, 0, math.Inf(-1)
		for j := range weights {
			weight := filterWeight((float64(s.start+j)-center)/scale, filter) / sum
			if weight > largestWeight {
				largest, largestWeight = j, weight
			}
			fixed := int(math.Round(weight * (1 << coefficientBits)))
			if fixed < -32768 || fixed > 32767 {
				return fmt.Errorf("%w: coefficient range", ErrOverflow)
			}
			weights[j] = int16(fixed)
			fixedSum += fixed
		}
		adjusted := int(weights[largest]) + (1 << coefficientBits) - fixedSum
		if adjusted < -32768 || adjusted > 32767 {
			return fmt.Errorf("%w: normalized coefficient range", ErrOverflow)
		}
		weights[largest] = int16(adjusted)
		l1 := 0
		for _, weight := range weights {
			v := int(weight)
			if v < 0 {
				v = -v
			}
			l1 += v
			if l1 > 32768 {
				return fmt.Errorf("%w: coefficient absolute sum", ErrOverflow)
			}
		}
	}
	// Trim only after normalization and validation: rounding and the sum
	// correction can turn a small boundary weight into a nonzero coefficient.
	// Retain the backing offsets instead of copying the remaining coefficients.
	// The normalized sum guarantees that every span retains at least one tap.
	maxTaps := 0
	for x, s := range p.spans {
		weights := p.coefficients[s.offset : s.offset+s.count]
		first, last := 0, len(weights)
		for weights[first] == 0 {
			first++
		}
		for weights[last-1] == 0 {
			last--
		}
		s.start += first
		s.offset += first
		s.count = last - first
		p.spans[x] = s
		if s.count > maxTaps {
			maxTaps = s.count
		}
	}
	p.source, p.destination, p.filter = source, destination, filter
	p.maxTaps = maxTaps
	return nil
}

func filterWeight(x float64, filter Filter) float64 {
	x = math.Abs(x)
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

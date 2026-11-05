package process

import (
	"errors"
	"fmt"
	"math/bits"
)

type geometry struct{ width, height, resizeWidth, resizeHeight, x, y int }

func checkPixels(w, h int, limit int64) error {
	maxInt := int(^uint(0) >> 1)
	if w <= 0 || h <= 0 || w > maxInt/4 || h > maxInt/4/w {
		return errors.New("dimensions overflow RGBA storage")
	}
	if int64(w) > limit/int64(h) {
		return errors.New("pixel limit exceeded")
	}
	return nil
}

// ratioCompare compares a/b and c/d without overflowing machine integers.
func ratioCompare(a, b, c, d int) int {
	ah, al := bits.Mul64(uint64(a), uint64(d))
	bh, bl := bits.Mul64(uint64(c), uint64(b))
	if ah < bh || (ah == bh && al < bl) {
		return -1
	}
	if ah > bh || (ah == bh && al > bl) {
		return 1
	}
	return 0
}
func scaleDimension(value, numerator, denominator int, ceil bool) (int, error) {
	hi, lo := bits.Mul64(uint64(value), uint64(numerator))
	if hi >= uint64(denominator) {
		return 0, errors.New("dimension overflow")
	}
	q, r := bits.Div64(hi, lo, uint64(denominator))
	if q > uint64(int(^uint(0)>>1)) {
		return 0, errors.New("dimension overflow")
	}
	if (ceil && r != 0) || (!ceil && r >= (uint64(denominator)+1)/2) {
		q++
	}
	if q > uint64(int(^uint(0)>>1)) {
		return 0, errors.New("dimension overflow")
	}
	if q == 0 {
		q = 1
	}
	return int(q), nil
}
func plan(sw, sh int, o Options, limit int64) (geometry, error) {
	g := geometry{width: o.Width, height: o.Height, resizeWidth: o.Width, resizeHeight: o.Height}
	if o.Mode == Exact {
		if !o.AllowUpscale && (o.Width > sw || o.Height > sh) {
			return g, errors.New("process: exact would upscale")
		}
	} else {
		// Choose the controlling ratio. Fit minimizes it; fill maximizes it.
		n, d := o.Width, sw
		if o.Width == 0 || (o.Height != 0 && ((o.Mode == Fit && ratioCompare(o.Height, sh, n, d) < 0) || (o.Mode == Fill && ratioCompare(o.Height, sh, n, d) > 0))) {
			n, d = o.Height, sh
		}
		if !o.AllowUpscale && n > d {
			if o.Mode == Fill {
				return g, errors.New("process: fill would upscale")
			}
			n, d = 1, 1
		}
		var err error
		g.resizeWidth, err = scaleDimension(sw, n, d, o.Mode == Fill)
		if err != nil {
			return g, err
		}
		g.resizeHeight, err = scaleDimension(sh, n, d, o.Mode == Fill)
		if err != nil {
			return g, err
		}
		if o.Mode == Fit {
			g.width, g.height = g.resizeWidth, g.resizeHeight
		}
	}
	if err := checkPixels(g.width, g.height, limit); err != nil {
		return g, fmt.Errorf("process: output: %w", err)
	}
	if err := checkPixels(g.resizeWidth, g.resizeHeight, limit); err != nil {
		return g, fmt.Errorf("process: intermediate output: %w", err)
	}
	dx, dy := g.resizeWidth-g.width, g.resizeHeight-g.height
	g.x, g.y = dx/2, dy/2
	switch o.Anchor {
	case TopLeft, Left, BottomLeft:
		g.x = 0
	case TopRight, Right, BottomRight:
		g.x = dx
	}
	switch o.Anchor {
	case TopLeft, Top, TopRight:
		g.y = 0
	case BottomLeft, Bottom, BottomRight:
		g.y = dy
	}
	return g, nil
}

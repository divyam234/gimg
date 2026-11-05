package process

import (
	"context"
	"encoding/binary"
	"image"
	"image/draw"

	"github.com/gen2brain/jpegn"
)

// pixelTransform is the linear part of a rectangle's orthogonal transform.
// Translation is derived from the negative axes and source dimensions.
// Every row/column has exactly one nonzero entry, equal to 1 or -1.
type pixelTransform struct{ a, b, c, d int }

var exifTransforms = [9]pixelTransform{
	{},
	{1, 0, 0, 1},   // 1: identity
	{-1, 0, 0, 1},  // 2: horizontal mirror
	{-1, 0, 0, -1}, // 3: 180 degrees
	{1, 0, 0, -1},  // 4: vertical mirror
	{0, 1, 1, 0},   // 5: transpose
	{0, -1, 1, 0},  // 6: 90 degrees clockwise
	{0, -1, -1, 0}, // 7: transverse
	{0, 1, -1, 0},  // 8: 270 degrees clockwise
}

// then composes source->t->next, so explicit axes follow EXIF orientation.
func (t pixelTransform) then(next pixelTransform) pixelTransform {
	return pixelTransform{
		next.a*t.a + next.b*t.c, next.a*t.b + next.b*t.d,
		next.c*t.a + next.d*t.c, next.c*t.b + next.d*t.d,
	}
}

func imageTransform(exif int, o Options) pixelTransform {
	t := exifTransforms[1]
	if !o.DisableAutoOrient && exif >= 1 && exif <= 8 {
		t = exifTransforms[exif]
	}
	switch o.Rotate {
	case 90:
		t = t.then(exifTransforms[6])
	case 180:
		t = t.then(exifTransforms[3])
	case 270:
		t = t.then(exifTransforms[8])
	}
	if o.FlipHorizontal {
		t = t.then(exifTransforms[2])
	}
	if o.FlipVertical {
		t = t.then(exifTransforms[4])
	}
	return t
}

func (t pixelTransform) size(w, h int) (int, int) {
	if t.a == 0 {
		return h, w
	}
	return w, h
}

func (t pixelTransform) point(x, y, w, h int) (int, int) {
	dx, dy := t.a*x+t.b*y, t.c*x+t.d*y
	if t.a < 0 {
		dx += w - 1
	}
	if t.b < 0 {
		dx += h - 1
	}
	if t.c < 0 {
		dy += w - 1
	}
	if t.d < 0 {
		dy += h - 1
	}
	return dx, dy
}

// orientImage performs at most one source-sized RGBA allocation, even when EXIF,
// rotation, and flips are combined. Identity compositions retain the native image
// and existing lazy JPEG-row path. JPEG conversion uses the codec's exact row
// converter once per source row, never a second full-sized conversion buffer.
func orientImage(ctx context.Context, img image.Image, t pixelTransform) (image.Image, error) {
	if t == exifTransforms[1] {
		return img, nil
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	dw, dh := t.size(w, h)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	var rowImage *image.RGBA
	var scratch []byte
	if _, ok := img.(*image.RGBA); !ok {
		rowImage = image.NewRGBA(image.Rect(0, 0, w, 1))
		if _, ok := img.(*image.YCbCr); ok {
			scratch = make([]byte, 2*w)
		}
	}
	step := 4*t.a + dst.Stride*t.c
	for y := range h {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row []byte
		switch src := img.(type) {
		case *image.RGBA:
			row = src.Pix[y*src.Stride:][:4*w]
		case *image.YCbCr:
			row = rowImage.Pix
			if err := jpegn.YCbCrToRGBARow(row, scratch, src, bounds.Min.Y+y); err != nil {
				return nil, err
			}
		default:
			draw.Draw(rowImage, rowImage.Bounds(), img, image.Pt(bounds.Min.X, bounds.Min.Y+y), draw.Src)
			row = rowImage.Pix
		}
		dx, dy := t.point(0, y, w, h)
		index := dy*dst.Stride + 4*dx
		for x := 0; x < len(row); x += 4 {
			binary.LittleEndian.PutUint32(dst.Pix[index:index+4], binary.LittleEndian.Uint32(row[x:x+4]))
			index += step
		}
	}
	return dst, nil
}

package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"

	resize "github.com/divyam234/gimg"
	"github.com/gen2brain/jpegn"
	"github.com/gen2brain/pngn"
	"github.com/gen2brain/vpx/webp"
)

var errInputLimit = errors.New("process: input byte limit exceeded")

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(b)
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
	n   int64
}

func (w *contextWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.w.Write(b)
	w.n += int64(n)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = w.ctx.Err()
	}
	return n, err
}

// boundedInput probes at most one byte beyond the limit, without overflowing
// limit+1. Replay bytes never pass through this counter a second time.
type boundedInput struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (r *boundedInput) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if r.exceeded {
		return 0, errInputLimit
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.r.Read(probe[:])
		if n > 0 {
			r.exceeded = true
			return 0, errInputLimit
		}
		return 0, err
	}
	if int64(len(b)) > r.remaining {
		b = b[:int(r.remaining)]
	}
	n, err := r.r.Read(b)
	r.remaining -= int64(n)
	return n, err
}

type jpegRows struct{ *image.YCbCr }

func (s jpegRows) ScratchSize() int { return 2 * s.Rect.Dx() }
func (s jpegRows) ReadRGBARow(dst, scratch []byte, y int) error {
	return jpegn.YCbCrToRGBARow(dst, scratch, s.YCbCr, y)
}

func (p *Processor) acquire(ctx context.Context) (*resize.Resizer, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case r := <-p.pool:
		if err := ctx.Err(); err != nil {
			p.pool <- r
			return nil, nil, err
		}
		return r, func() { p.pool <- r }, nil
	}
}
func (p *Processor) transform(ctx context.Context, dst io.Writer, src io.Reader, o Options) (Result, error) {
	r, release, err := p.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	return p.transformWithResizer(ctx, dst, src, o, r)
}
func (p *Processor) transformWithResizer(ctx context.Context, dst io.Writer, src io.Reader, o Options, resizer *resize.Resizer) (Result, error) {
	if src == nil || dst == nil {
		return Result{}, errors.New("process: nil reader or writer")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	input := &boundedInput{r: contextReader{ctx, src}, remaining: p.cfg.MaxInputBytes}
	var metadata *orientationReader
	var encoded io.Reader = input
	if !o.DisableAutoOrient {
		metadata = newOrientationReader(input)
		encoded = metadata
	}
	reader := bufio.NewReader(encoded)
	header, _ := reader.Peek(32)
	var format Format
	switch {
	case len(header) >= 2 && header[0] == 0xff && header[1] == 0xd8:
		format = JPEG
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		format = PNG
	case len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP":
		format = WebP
	case isHEICHeader(header):
		format = HEIC
	default:
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if input.exceeded {
			return Result{}, errInputLimit
		}
		return Result{}, errors.New("process: unsupported or corrupt input; expected JPEG, WebP, PNG, or HEIC")
	}
	var prefix bytes.Buffer
	inspected := io.TeeReader(reader, &prefix)
	var config image.Config
	var err error
	switch format {
	case JPEG:
		config, err = jpegn.DecodeConfig(inspected)
	case PNG:
		config, err = pngn.DecodeConfig(inspected)
	case WebP, HEIC:
		// Both codecs need random access. Buffer bounded input once and
		// expose ReaderAt/Seeker to config inspection and full decoding.
		if _, err = io.Copy(&prefix, reader); err == nil {
			if format == WebP {
				config, err = webp.DecodeConfig(bytes.NewReader(prefix.Bytes()))
			} else {
				config, err = heicConfig(prefix.Bytes())
			}
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("process: decode config: %w", err)
	}
	if err = checkPixels(config.Width, config.Height, p.cfg.MaxSourcePixels); err != nil {
		return Result{}, fmt.Errorf("process: source: %w", err)
	}
	replay := io.MultiReader(bytes.NewReader(prefix.Bytes()), reader)
	var img image.Image
	switch format {
	case JPEG:
		denominator := 1
		if o.JPEGShrink {
			// DecodeConfig can stop at SOF before a later APP1 segment. Read
			// through the remaining header before choosing scaled decoding,
			// replaying captured bytes instead of seeking the source.
			if metadata != nil {
				var chunk [4096]byte
				for !metadata.JPEGHeaderDone() {
					n, readErr := reader.Read(chunk[:])
					prefix.Write(chunk[:n])
					if readErr != nil {
						if readErr == io.EOF {
							break
						}
						return Result{}, fmt.Errorf("process: read JPEG header: %w", readErr)
					}
				}
				replay = io.MultiReader(bytes.NewReader(prefix.Bytes()), reader)
			}
			orientation := 1
			if metadata != nil {
				orientation = metadata.Orientation()
			}
			t := imageTransform(orientation, o)
			sw, sh := t.size(config.Width, config.Height)
			g, planErr := plan(sw, sh, o, p.cfg.MaxOutputPixels)
			if planErr != nil {
				return Result{}, planErr
			}
			for _, d := range []int{8, 4, 2} {
				dw, dh := t.size((config.Width-1)/d+1, (config.Height-1)/d+1)
				if dw >= g.resizeWidth && dh >= g.resizeHeight {
					denominator = d
					break
				}
			}
		}
		img, err = jpegn.Decode(replay, &jpegn.Options{ToRGBA: config.ColorModel != color.YCbCrModel, ScaleDenom: denominator})
	case PNG:
		img, err = pngn.Decode(replay, &pngn.Options{
			PixelLimit: int(min(p.cfg.MaxSourcePixels, int64(int(^uint(0)>>1)))),
		})
	case WebP:
		img, err = webp.Decode(bytes.NewReader(prefix.Bytes()), webp.Options{ToRGBA: true, Threads: p.cfg.Workers})
	case HEIC:
		img, err = decodeHEIC(prefix.Bytes(), p.cfg.Workers, p.cfg.MaxSourcePixels, !o.DisableAutoOrient)
	}
	if err != nil {
		return Result{}, fmt.Errorf("process: decode: %w", err)
	}
	// Some decoders stop at an end marker. Still enforce the bound on the whole
	// supplied stream, including trailing bytes and bytes buffered during probing.
	if _, err = io.Copy(io.Discard, reader); err != nil {
		return Result{}, fmt.Errorf("process: read input: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	orientation := 1
	if metadata != nil {
		orientation = metadata.Orientation()
	}
	if format == HEIC {
		// Native HEIF presentation can rotate/crop the coded item. Use the
		// presented bounds for explicit transforms and resize planning.
		config.Width, config.Height = img.Bounds().Dx(), img.Bounds().Dy()
	}
	t := imageTransform(orientation, o)
	sw, sh := t.size(config.Width, config.Height)
	g, err := plan(sw, sh, o, p.cfg.MaxOutputPixels)
	if err != nil {
		return Result{}, err
	}
	img, err = orientImage(ctx, img, t)
	if err != nil {
		return Result{}, fmt.Errorf("process: orient: %w", err)
	}
	var out *image.RGBA
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Dx() == g.resizeWidth && rgba.Rect.Dy() == g.resizeHeight {
		// Decoded/oriented storage is job-owned; an equal-size transform
		// can encode it directly instead of copying another full image.
		out = rgba
	} else {
		out = image.NewRGBA(image.Rect(0, 0, g.resizeWidth, g.resizeHeight))
		switch s := img.(type) {
		case *image.YCbCr:
			err = resizer.ResizeRows(out, jpegRows{s}, o.Filter)
		case *image.RGBA:
			err = resizer.Resize(out, s, o.Filter)
		default:
			b := s.Bounds()
			rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
			draw.Draw(rgba, rgba.Bounds(), s, b.Min, draw.Src)
			err = resizer.Resize(out, rgba, o.Filter)
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("process: resize: %w", err)
	}
	if g.width != g.resizeWidth || g.height != g.resizeHeight {
		out = out.SubImage(image.Rect(g.x, g.y, g.x+g.width, g.y+g.height)).(*image.RGBA)
	}
	if o.Encode.Format == JPEG {
		// The resize buffer is private to this job. Composite in place rather than
		// allocating another full-sized image solely to remove alpha.
		bg := o.Encode.Background
		for y := range out.Rect.Dy() {
			row := out.Pix[y*out.Stride:][:out.Rect.Dx()*4]
			for x := 0; x < len(row); x += 4 {
				inverse := uint32(255 - row[x+3])
				row[x] += uint8((uint32(bg.R)*inverse + 127) / 255)
				row[x+1] += uint8((uint32(bg.G)*inverse + 127) / 255)
				row[x+2] += uint8((uint32(bg.B)*inverse + 127) / 255)
				row[x+3] = 255
			}
		}
	}
	writer := &contextWriter{ctx: ctx, w: dst}
	switch o.Encode.Format {
	case PNG:
		err = pngn.Encode(writer, out)
	case JPEG:
		err = jpegn.Encode(writer, out, &jpegn.EncodeOptions{Quality: o.Encode.Quality, Subsampling: jpegn.Subsample420, Progressive: false})
	case WebP:
		err = webp.Encode(writer, out, webp.EncodeOptions{Quality: o.Encode.Quality, Lossless: o.Encode.WebPLossless, Method: o.Encode.WebPMethod, Threads: p.cfg.Workers})
	case HEIC:
		err = encodeHEIC(writer, out, o.Encode.Quality)
	}
	result := Result{Width: g.width, Height: g.height, Format: o.Encode.Format, BytesWritten: writer.n}
	if err != nil {
		return result, fmt.Errorf("process: encode: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

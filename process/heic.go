package process

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"io"

	nativeheic "github.com/gen2brain/h265/heic"
)

// isHEICHeader recognizes HEVC and generic HEIF brands. Generic HEIF files
// still need native codec validation; AVIF-only compatible brands are excluded.
func isHEICHeader(data []byte) bool {
	if len(data) < 16 || string(data[4:8]) != "ftyp" {
		return false
	}
	size := uint64(binary.BigEndian.Uint32(data[:4]))
	if size < 16 || (size-16)%4 != 0 {
		return false
	}
	var hevc, generic, avif bool
	brand := func(b []byte) {
		switch string(b) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs":
			hevc = true
		case "mif1", "msf1":
			generic = true
		case "avif", "avis":
			avif = true
		}
	}
	brand(data[8:12])
	end := min(uint64(len(data)), size)
	for i := uint64(16); i+4 <= end; i += 4 {
		brand(data[i : i+4])
	}
	return hevc || generic && !avif
}

func heicConfig(data []byte) (image.Config, error) {
	return nativeheic.DecodeConfig(bytes.NewReader(data))
}

func decodeHEIC(data []byte, workers int, maxSourcePixels int64, autoOrient bool) (image.Image, error) {
	// Bound coded images (including auxiliary images and grid tiles), not only
	// the presented dimensions returned by DecodeConfig. Clamp before converting
	// to int so a configured int64 limit cannot disable the codec's allocation cap.
	limit := min(maxSourcePixels, int64(int(^uint(0)>>1)))
	if limit <= 0 {
		return nil, fmt.Errorf("process: HEIC source pixel limit must be positive")
	}
	img, err := nativeheic.Decode(bytes.NewReader(data), nativeheic.Options{
		Threads: workers, FrameSizeLimit: int(limit), AutoRotate: autoOrient,
	})
	if err != nil {
		return nil, err
	}
	if err := checkPixels(img.Bounds().Dx(), img.Bounds().Dy(), maxSourcePixels); err != nil {
		return nil, fmt.Errorf("process: HEIC source: %w", err)
	}
	return img, nil
}

func encodeHEIC(dst io.Writer, img image.Image, quality int) error {
	return nativeheic.Encode(dst, img, nativeheic.EncodeOptions{Quality: quality})
}

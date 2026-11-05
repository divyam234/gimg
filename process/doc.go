// Package process implements an embeddable, bounded image-processing pipeline
// without a server. It accepts JPEG, PNG, WebP, and HEIC by signature from
// non-seekable readers and writes JPEG, PNG, WebP, or HEIC to the caller's writer.
// The processor does not close caller-provided readers, writers, or its optional
// disk cache.
//
// Construct a reusable processor with New, and close it when the application
// shuts down. Options default to Exact, Bicubic, PNG, and no upscaling. JPEG,
// lossy WebP, and HEIC quality default to 85. WebP method zero is a valid explicit
// speed/quality choice. For example, fitting an already-open reader:
//
//	p, err := process.New(process.Config{Concurrency: 2, Workers: 2})
//	if err != nil {
//		return err
//	}
//	defer p.Close()
//	_, err = p.Transform(ctx, dst, src, process.Options{
//		Width: 1280,
//		Mode: process.Fit,
//		Encode: process.EncodeOptions{
//			Format: process.WebP,
//			Quality: 85,
//			WebPMethod: 4,
//		},
//	})
//
// Here src is an io.Reader and dst is an io.Writer owned by the caller. Output
// can be partially written on error; callers requiring atomic destination files
// should write a same-directory temporary file and rename after successful close.
//
// For caching, open a diskcache.Cache and pass it in Config.Cache. TransformCached
// accepts a Source with a nonempty Key, Version, and context-aware Open callback.
// A hit avoids opening or reading the source. Change Version whenever the source
// bytes change; identity is supplied by the application, not inferred from a URL,
// file path, timestamp, or arbitrary reader. Normalized options also participate
// in the cache key. Code and codec upgrades do not invalidate existing entries;
// prune the cache or change the source Version when regeneration is required.
//
// Cache failures are returned with ErrCache rather than silently ignored. Shared
// misses within one processor reuse generation; cancellation of its generating
// caller fails those waiters too. A later call may retry. Source/input limits
// apply during generation, while output dimensions are also checked on cache
// hits. Close the processor before closing an application-owned shared cache.
// Disk-cache byte limits require explicit Prune or an opted-in cleanup interval.
//
// Exact resizes to both requested dimensions without preserving aspect ratio.
// Fit preserves aspect ratio inside the supplied box; either dimension may be
// omitted. Fit rounds to the nearest pixel (ties upward), with a minimum of one
// pixel. Fill preserves aspect ratio, rounds the intermediate dimensions upward,
// then crops to the requested rectangle using one of nine anchors. Center crops
// put an unmatched odd pixel on the right or bottom. Upscaling is disabled by
// default: Exact and Fill reject it, while Fit caps the scale at the source size.
// Intermediate Fill images are subject to the output pixel limit as well.
//
// Auto-orientation is enabled by default. JPEG, PNG, and WebP use EXIF orientation;
// all eight EXIF orientations are supported, and malformed or unsupported
// orientation metadata is ignored. HEIC instead uses its container presentation
// transforms (clean aperture, rotation, and mirroring); its informational EXIF
// orientation is not applied a second time. DisableAutoOrient disables automatic
// orientation, although the HEIC decoder still applies representable coded clean
// apertures. HEIC geometry is planned from the presented decoded bounds, not the
// unrotated header dimensions. Before resizing, fitting, filling, or cropping,
// transforms run in this order: automatic orientation, Rotate (clockwise degrees,
// a multiple of 90), FlipHorizontal, then FlipVertical. Flip axes refer to the
// image after rotation. Quarter turns exchange the source width and height used
// for geometry planning.
//
// Input byte limits cover the complete stream, including trailing data. Header
// bytes consumed by DecodeConfig are replayed rather than sought or counted
// twice. Source dimensions are checked before full decoding; output dimensions
// are checked before allocation. WebP and HEIC input is buffered within the input
// byte limit for random-access decoding; the same encoded buffer is shared by
// header inspection and full decoding. PNG's codec buffers the encoded input
// during full decoding. These encoded buffers are additional to decoded pixel
// storage. JPEG decoding materializes a complete RGBA image using the upstream
// codec. The processor's EXIF/explicit transform composition uses at most one
// source-sized orientation buffer, plus row scratch, rather than one per operation.
// HEIC's native container presentation may allocate additional transform buffers.
// Optional JPEG scaled decoding is chosen against the planned intermediate size;
// JPEG geometry is always computed from the oriented original source dimensions.
//
// PNG, WebP, and HEIC retain alpha; HEIC alpha is encoded lossily. JPEG composites
// premultiplied pixels over an opaque background, defaulting to white. Processing
// uses premultiplied 8-bit RGBA, including when HEIC decodes a higher-bit-depth
// source. Filtering takes place in encoded color space without gamma correction;
// no ICC profile, color-primary, or transfer-function conversion is performed.
// Source metadata is not preserved in the newly encoded output. This API targets
// static images, not animation, HTTP routing, URL signing, or remote-source
// retrieval.
//
// JPEG uses github.com/gen2brain/jpegn, PNG uses github.com/gen2brain/pngn,
// WebP uses github.com/gen2brain/vpx/webp, and HEIC uses
// github.com/gen2brain/h265/heic. All are native Go codecs with SIMD acceleration
// on supported CPUs and CPU fallbacks; no CGo, WebAssembly, or shared libraries
// are required. PNG decoding is strict: Partial and IgnoreCRC are both false.
//
// Each Processor owns Concurrency reusable resizers, each with Workers workers.
// WebP codec threads and HEIC decode threads use the same per-job Workers bound.
// Close rejects new calls and waits for active calls, including calls waiting for
// a pool slot. Cancellation is checked while waiting and at reader/writer
// boundaries; it cannot forcibly interrupt a blocked arbitrary reader, writer,
// or codec invocation.
package process

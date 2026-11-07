# gimg vs libvips: production-style JPEG/PNG to WebP benchmark

**Run date:** 2026-11-07 · **Repository:** `github.com/divyam234/gimg` · **Revision:** `3cebf09`
**Result:** libvips wins 5 of 6 production-mode latency scenarios. gimg wins the *single-request* transparent-PNG thumbnail case, but the two-in-flight throughput is effectively tied and gimg has higher peak process RSS and larger files. JPEG full decoding without shrink is an additional gimg regression.

## Scope and environment

This is a **controlled, warmed service-process experiment**, **not a deployed HTTP production trace**. Each request reopens a real image, decodes it, resizes it, and encodes WebP to an in-memory byte buffer. No network, image cache hits, HTTP overhead, disk output writes, or response streaming are timed. File reads use a warm OS page cache. The libvips operation cache was disabled with `vips_cache_set_max(0)` so every request transforms its input.

- Hardware: Linux ARM64, Neoverse-N1; 12 visible CPUs, **2-CPU cgroup quota**; 23 GiB physical RAM.
- Toolchain: Go **1.26.10**; libvips **8.18.7** from Nix; WebP quality `85`, encoder effort/method `4`.
- For single-request results: one persistent process, **2 codec/resize workers**, 5 warmups then **35 measured transforms** per engine and scenario. That is **490 timed transformations** overall; both engines' source outputs and dimensions were verified.
- For two-request throughput: two persistent child processes, one worker per process, 2 warmups + 20 measured requests per process, 3 independent rounds per engine per scenario (**120 measured requests/engine/scenario**). Effective requests/sec uses end-to-end wall time **including warmups/startup** and is conservative.
- Latency confidence intervals are 95% nonparametric bootstrap CIs from 5,000 resamples of the 35 measurements, **conditional on these same inputs and this host**. They are not a confidence interval for arbitrary images or for deployment throughput. Request p95 is a sample estimate with considerable uncertainty at n=35.
- gimg uses `process.Processor.Transform`, Lanczos3 and optional JPEG scaled decode (`JPEGShrink`). libvips uses `vips_thumbnail` (shrink-on-load/block-shrink/Lanczos3) and `vips_webpsave_buffer` (Q=85, effort=4). **These produce different pixels; quality is not identical.**

## Source fixtures

These are real photographic inputs, with derived PNGs:

| Input | Origin / construction | Dimensions | SHA-256 |
|---|---|---|---|
| `landscape.jpg` | https://picsum.photos/id/10/4000/3000.jpg | 4000×3000 | `623f3d284cc7b79dd9dade66778dddf1930c9c14352e29b25966ed35a47173f9` |
| `architecture.jpg` | https://picsum.photos/id/29/4000/3000.jpg | 4000×3000 | `07b6e88509af60b45e3d33c8c98bdb1c55f2079a3b6aa24ff7968731e2f5d313` |
| `landscape.png` | `magick landscape.jpg -define png:compression-level=6 landscape.png`; opaque photograph | 4000×3000 | `7cd9253705483a07caabe039ce8e7e083617900d51ca30fa9f6949033417ca23` |
| `transparent.png` | `magick architecture.jpg -resize 3000x2250 -alpha set -background none -gravity center -extent 4000x3000 PNG32:transparent.png`; photographic content on a transparent margin | 4000×3000 | `2af1315f58b4d66541798ca6dadf3df13493ccd8c61cf4360b300c5ac16bdfd4` |

The alpha case is **synthetic cutout geometry around a real photograph**, not a representative sample of arbitrary product photos.

## Single-request latency (35 timed runs per engine)

Lower means/p95 are better. Ratio gimg/libvips **above 1.00 means libvips is faster**.

| Input → output | gimg mean ms [95% CI] | libvips mean ms [95% CI] | Ratio [95% CI] | p95 ms gimg / libvips |
|---|---:|---:|---:|---:|
| JPEG landscape → 320×240 WebP, JPEG shrink | 183.89 [180.86, 186.68] | 130.35 [126.40, 134.52] | 1.411 [1.364, 1.461] | 196.08 / 148.95 |
| JPEG architecture → 320×240 WebP, JPEG shrink | 275.05 [269.93, 280.56] | 177.01 [172.11, 182.00] | 1.554 [1.501, 1.608] | 301.17 / 200.97 |
| JPEG landscape → 1280×960 WebP, JPEG shrink | 482.88 [473.59, 492.84] | 462.69 [455.39, 470.40] | 1.044 [1.017, 1.072] | 537.18 / 502.40 |
| Opaque photographic PNG → 320×240 WebP | 348.41 [342.14, 354.95] | 286.56 [282.00, 291.30] | 1.216 [1.186, 1.245] | 378.82 / 311.41 |
| Transparent photographic PNG → 320×240 WebP | 377.56 [371.62, 383.74] | 419.88 [413.39, 426.73] | 0.899 [0.879, 0.919] | 406.93 / 456.89 |
| Transparent photographic PNG → 1280×960 WebP | 921.21 [914.08, 928.45] | 899.94 [887.86, 913.63] | 1.024 [1.007, 1.039] | 956.11 / 932.89 |
| JPEG landscape → 320×240 WebP, **gimg full JPEG decode** | 351.33 [325.68, 384.67] | 129.34 [124.79, 133.99] | 2.716 [2.489, 3.005] | 494.15 / 156.64 |

The full JPEG-decode gimg case had substantial jitter (**26.9% coefficient of variation**). Its mean is clearly slower, but its p95 is less precisely estimated.

## Output bytes, process memory, and quality

Peak RSS is **process lifetime high-water RSS after 40 requests**, **not per-request allocated memory**. It is influenced by allocator retention, garbage collection, runtime startup and C library caches. The two implementations have different memory models. Bigger WebP outputs do not imply better visuals without quality verification.

| Scenario | WebP bytes gimg / libvips | gimg byte overhead | Peak RSS MiB gimg / libvips |
|---|---:|---:|---:|
| JPEG landscape thumb | 14,178 / 13,914 | +1.9% | 170.8 / 234.9 |
| JPEG architecture thumb | 28,434 / 27,928 | +1.8% | 178.7 / 256.8 |
| JPEG landscape medium | 235,872 / 218,292 | +8.1% | 206.8 / 242.7 |
| Opaque PNG thumb | 14,930 / 13,562 | +10.1% | 275.9 / 121.3 |
| Transparent PNG thumb | 19,208 / 17,218 | +11.6% | 353.1 / 154.1 |
| Transparent PNG medium | 272,970 / 242,050 | +12.8% | 365.6 / 178.2 |
| JPEG full decode thumb | 14,948 / 13,914 | +7.4% | 306.8 / 215.7 |

Every output was confirmed to have the expected geometry; WebP outputs from transparent PNG retained an alpha channel. Fidelity was checked using **ImageMagick Lanczos-resized PNG references**; this is a proxy, **not a perceptual or matched-quality evaluation**. Reference PSNR, in dB (gimg / libvips):

- JPEG landscape thumbnail: **35.51 / 35.76**
- JPEG architecture thumbnail: **34.06 / 35.64**
- JPEG landscape medium: **38.89 / 38.31**
- Opaque PNG thumbnail: **36.06 / 35.65**
- Transparent PNG thumbnail, composited on white: **38.53 / 36.65** (on black: 38.53 / 36.74)
- Transparent PNG medium, white or black: **39.87 / 38.44**

Therefore the transparent PNG speed win should be interpreted alongside gimg's **11.6% larger file** and higher reference PSNR. Equal Q=85 does not guarantee equal visual fidelity.
## Consolidated results: full metric table

This is the baseline comparison of the **unmodified** `3cebf09` implementation. Each row has **35 timed samples per engine**; mean latency CIs are 95% bootstrap intervals. Values are **gimg / libvips** unless otherwise marked. A ratio above 1 means libvips was faster.

| Source → WebP output | Mean ms: gimg / libvips | p95 ms: gimg / libvips | Mean gimg/vips ratio [95% CI] | Output bytes: gimg / libvips | Peak RSS MiB: gimg / libvips | Faster mean |
|---|---:|---:|---:|---:|---:|---|
| JPEG landscape → 320×240, scaled decode | 183.89 / 130.35 | 196.08 / 148.95 | 1.411 [1.364, 1.461] | 14,178 / 13,914 | 170.8 / 234.9 | libvips |
| JPEG architecture → 320×240, scaled decode | 275.05 / 177.01 | 301.17 / 200.97 | 1.554 [1.501, 1.608] | 28,434 / 27,928 | 178.7 / 256.8 | libvips |
| JPEG landscape → 1280×960, scaled decode | 482.88 / 462.69 | 537.18 / 502.40 | 1.044 [1.017, 1.072] | 235,872 / 218,292 | 206.8 / 242.7 | libvips |
| Opaque photo PNG → 320×240 | 348.41 / 286.56 | 378.82 / 311.41 | 1.216 [1.186, 1.245] | 14,930 / 13,562 | 275.9 / 121.3 | libvips |
| Transparent photo PNG → 320×240 | 377.56 / 419.88 | 406.93 / 456.89 | 0.899 [0.879, 0.919] | 19,208 / 17,218 | 353.1 / 154.1 | gimg |
| Transparent photo PNG → 1280×960 | 921.21 / 899.94 | 956.11 / 932.89 | 1.024 [1.007, 1.039] | 272,970 / 242,050 | 365.6 / 178.2 | libvips |
| JPEG landscape → 320×240, **full decode** | 351.33 / 129.34 | 494.15 / 156.64 | 2.716 [2.489, 3.005] | 14,948 / 13,914 | 306.8 / 215.7 | libvips |

**Qualification:** Process peak RSS is a lifetime high-water mark and can be affected by prior requests and garbage-collector/allocator behavior; the p95 estimates are from only 35 requests per engine. WebP quality numbers are equal, but perceptual fidelity was **not** matched.


## Two simultaneous requests: throughput

Two independent persistent processes with **1 worker each**, on the same **2-CPU quota**, each handling 20 measured requests per round. Three rounds per engine and input. QPS includes startup and 2 warmup requests per process, so it is a conservative effective throughput rather than perfectly isolated steady-state QPS.

| Scenario | gimg mean QPS (three-run range) | libvips mean QPS (range) | Measured-request mean latency ms gimg / libvips | Measured-request p95 ms gimg / libvips |
|---|---:|---:|---:|---:|
| JPEG landscape thumbnail | 9.44 (8.79–9.78) | 13.14 (12.70–13.50) | 187.98 / 129.88 | 207.82 / 162.34 |
| Transparent PNG thumbnail | 4.30 (4.28–4.32) | 4.25 (4.17–4.30) | 410.80 / 413.26 | 440.12 / 450.12 |

The transparent-PNG **single-request gimg speed win nearly disappears when two requests compete**. Throughput differences of ~1% in this case should not be treated as a meaningful production advantage.
## Additional mixed four-process, real-image check

A separate controlled load test used **four independent persistent worker processes**, each with one processing worker and a different real internet image. Two were JPEG → WebP and two PNG → WebP; one PNG had transparency. Each process performed two warm-ups and eight measured conversions, across two rounds per engine (**64 measured requests per engine**). Input images were:

| Work item | Input dimensions | Output | WebP bytes: gimg / libvips |
|---|---:|---:|---:|
| Cloud landscape, JPEG | 4000×3000 | 320×240 | 9,360 / 8,696 |
| Green landscape, JPEG | 4160×3120 | 960×720 | 93,260 / 84,288 |
| Screenshot, PNG | 2560×1600 | 320×200 | 5,082 / 4,536 |
| Transparent logo, PNG | 1021×1058 | 695×720 | 130,722 / 112,458 |

The engines used WebP Q=85/method=4, and libvips output metadata was explicitly **stripped** to match gimg; otherwise EXIF payloads distort file-size comparisons. Inputs were publicly sourced, but the small fixed corpus and uneven task costs limit generalization. Mean throughput below counts only measured conversions in the numerator, while wall time **also includes process startup and warm-ups**, making it a conservative effective rate.

| Engine | Round 1 req/s | Round 2 req/s | Two-round average | Mean latency r1 / r2 (ms) | Observed p95 r1 / r2 (ms) | Sum of process peak RSS r1 / r2 (MiB) |
|---|---:|---:|---:|---:|---:|---:|
| gimg | 7.53 | 7.45 | 7.49 | 218.4 / 224.9 | 393.4 / 384.6 | 224.1 / 226.3 |
| libvips | 9.85 | 10.00 | 9.93 | 203.8 / 209.6 | 353.8 / 355.2 | 280.8 / 282.1 |

libvips achieved approximately **33% higher effective throughput** on this mix, while gimg had a lower **sum of per-process RSS high-water marks**. These RSS sums are not instantaneous machine-level peak usage, and the request percentiles are exploratory rather than statistically stable service SLOs. No HTTP server, networking, request queue, persistent storage, or CDN cache was included.

Reproduction helper: `.bench-local/mixed_quick_20261107.py`. The individual 35-request and 120-request/engine data are retained as JSON files under `.bench-local/` (see the [raw-data section](#reproducibility-and-raw-data)).


## Engineering priorities

1. **P0: reduce PNG full-image materialization and copies.** Inspect `process/codec.go:217–270` and whether `pngn.Decode` returns non-`*image.RGBA` images that take the full `image.NewRGBA`/`draw.Draw` fallback. Adapt the existing `RGBARowSource`/`ResizeRows` interface for eligible PNG row conversion, and measure transient peak allocations. This should be targeted at the 2×+ high-water RSS penalty on PNG.
2. **P0: keep JPEG shrink-on-load available and expose an automatic production mode.** `cmd/gimg/main.go:34` defaults it off. Note that libvips thumbnail deliberately uses shrink-on-load plus block and Lanczos processing, so fidelity must be tested at the target dimensions.
3. **P1: profile encode vs decode vs resize in WebP service cases.** The JPEG architecture thumbnail is 1.55× slower in gimg despite having a tuned resizer; profile `jpegn.Decode`, `Resize`, and `webp.Encode` separately before rewriting assembly.
4. **P1: compare file size *at matched visual quality*** (PSNR/SSIM/MS-SSIM/VMAF-like appropriate image metrics, preferably a human-reviewed corpus). Nominal Q85 and method4 are not enough to prove equivalent quality, and WebP output sizes differ by 2–13%.
5. **P1: test genuine service load.** Use varied real traffic inputs (JPEG orientations/progressive JPEGs, PNG alpha shapes, colorspace/ICC, mixed sizes) with concurrency, burst behavior, image fetch, cold cache, HTTP overhead, memory limits and p99. This experiment does not establish deployment-wide tail latency or memory budgeting.

**Conclusion:** On these measured real-photo and derived-PNG cases, use libvips as the baseline for a general JPEG/PNG→WebP service. gimg is interesting for transparent thumbnail latency but its advantage does not persist in the two-process QPS check, and its PNG memory footprint deserves investigation. No source implementation changes were made during this comparison.

## Reproducibility and raw data

Scripts and fixtures currently remain in `.bench-local/` (local benchmark scratch, **not tracked in Git**):

- `service_gimg.go` and `service_vips.c`: long-running-process per-transform timers.
- `run_bench.py`, `webp_results.json`: 35-run single-request test and **all 490 raw latency samples**.
- `quality_check.py`, `quality_results.json`: output geometry/alpha checks and reference PSNR.
- `concurrent_bench.py`, `concurrent_results.json`: three two-in-flight rounds per input/engine and **all 480 measured concurrent-request latencies**.
- `inputs/` and `outputs/`: test fixtures and all encoded WebP files.

Build gimg test driver with `go build -o .bench-local/service_gimg ./.bench-local/service_gimg.go`. Compile the native libvips test driver against Nix's `vips`/pkg-config dev environment (including an rpath to the installed libvips). The Python scripts require Python 3; the quality script also requires ImageMagick 7. Example run: `BENCH_RUNS=35 BENCH_WARMUP=5 BENCH_WORKERS=2 python3 .bench-local/run_bench.py`.

libvips algorithm references:
- https://www.libvips.org/API/8.17/ctor.Image.thumbnail.html
- https://www.libvips.org/API/8.17/method.Image.webpsave.html

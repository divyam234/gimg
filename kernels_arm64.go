//go:build arm64 && !purego

package gimg

func init() {
	horizontalImpl = horizontalNEON
	verticalImpl = verticalNEON
}

// horizontalNEON uses one four-lane RGBA packet per output pixel. Neither
// source taps nor coefficient loads read past their final element.
//
//go:noescape
func horizontalNEON(dst []int16, src []byte, spans []span, coefficients []int16)

func verticalNEON(dst []byte, rows [][]int16, coefficients []int16) {
	n := len(dst) &^ 15
	if n != 0 {
		verticalNEONPackets(dst[:n], rows, coefficients)
	}
	if n != len(dst) {
		verticalScalarRange(dst, rows, coefficients, n)
	}
}

// verticalNEONPackets requires len(dst) to be a multiple of 16 channels.
// Each row contains at least len(dst) int16 values; coefficients match rows.
// The validated core owns these invariants, as for the scalar kernels.
//
//go:noescape
func verticalNEONPackets(dst []byte, rows [][]int16, coefficients []int16)

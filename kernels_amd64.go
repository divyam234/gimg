//go:build amd64 && !purego

package gimg

import "golang.org/x/sys/cpu"

func init() {
	if cpu.X86.HasAVX2 {
		horizontalImpl = horizontalAVX2
		verticalImpl = verticalAVX2
	}
}

//go:noescape
func horizontalAVX2(dst []int16, src []byte, spans []span, coefficients []int16)

// verticalAVX2Blocks writes a whole number of eight-channel blocks. The wrapper
// keeps the final pixel outside assembly, so no vector load crosses a row end.
//
//go:noescape
func verticalAVX2Blocks(dst []byte, rows [][]int16, coefficients []int16)

func verticalAVX2(dst []byte, rows [][]int16, coefficients []int16) {
	n := len(dst) &^ 7
	if n != 0 {
		verticalAVX2Blocks(dst[:n], rows, coefficients)
	}
	if n != len(dst) {
		verticalScalarRange(dst, rows, coefficients, n)
	}
}

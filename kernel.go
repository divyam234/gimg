package gimg

// Coefficients sum to 1<<coefficientBits on every axis. Horizontal results
// retain five fractional bits, including signed ringing, until the final pass.
const (
	coefficientBits = 14
	fractionBits    = 5
	horizontalShift = coefficientBits - fractionBits
	verticalShift   = coefficientBits + fractionBits
)

// span identifies one output sample's contiguous source taps and coefficients.
// Native kernels rely on these three machine-sized fields in this order.
type span struct {
	start  int
	offset int
	count  int
}

type horizontalFunc func(dst []int16, src []byte, spans []span, coefficients []int16)
type verticalFunc func(dst []byte, rows [][]int16, coefficients []int16)

// Architecture-specific initialization replaces these once when supported.
// Scalar code remains available for other CPUs and the purego build tag.
var (
	horizontalImpl horizontalFunc = horizontalScalar
	verticalImpl   verticalFunc   = verticalScalar
)

//go:build amd64 && !purego

#include "textflag.h"

// Pair adjacent RGBA taps for VPMADDWD: [w0,w1] in the low lane,
// [w2,w3] in the high lane, repeated for each color channel.
DATA ·horizontalCoefficientShuffle+0(SB)/8, $0x0302010003020100
DATA ·horizontalCoefficientShuffle+8(SB)/8, $0x0302010003020100
DATA ·horizontalCoefficientShuffle+16(SB)/8, $0x0706050407060504
DATA ·horizontalCoefficientShuffle+24(SB)/8, $0x0706050407060504
GLOBL ·horizontalCoefficientShuffle(SB), RODATA|NOPTR, $32

DATA ·horizontalPixelShuffle+0(SB)/8, $0x0b0a030209080100
DATA ·horizontalPixelShuffle+8(SB)/8, $0x0f0e07060d0c0504
DATA ·horizontalPixelShuffle+16(SB)/8, $0x0b0a030209080100
DATA ·horizontalPixelShuffle+24(SB)/8, $0x0f0e07060d0c0504
GLOBL ·horizontalPixelShuffle(SB), RODATA|NOPTR, $32

DATA ·verticalAlphaShuffle+0(SB)/8, $0x0707070703030303
DATA ·verticalAlphaShuffle+8(SB)/8, $0x0f0f0f0f0b0b0b0b
GLOBL ·verticalAlphaShuffle(SB), RODATA|NOPTR, $16

// Four taps are consumed as one contiguous 16-byte RGBA packet. Adjacent
// tap products are summed directly into int32 lanes, without saturation.
// Keep the two tap-pair sums in separate YMM lanes until the packet loop ends.
// Every partial sum is bounded by 255*sum(abs(weights)) <= 255*32768,
// so regrouping the int32 additions is exact, including signed filter lobes.
TEXT ·horizontalAVX2(SB), NOSPLIT, $0-96
	MOVQ dst_base+0(FP), DI
	MOVQ src_base+24(FP), BX
	MOVQ spans_base+48(FP), SI
	MOVQ spans_len+56(FP), R8
	MOVQ coefficients_base+72(FP), R9
	VMOVDQU ·horizontalCoefficientShuffle(SB), Y7
	VMOVDQU ·horizontalPixelShuffle(SB), Y9
	MOVL $256, R10
	VMOVD R10, X6
	VPSHUFD $0, X6, X6
	TESTQ R8, R8
	JE horizontal_done

horizontal_pixel:
	MOVQ 0(SI), AX
	LEAQ (BX)(AX*4), AX
	MOVQ 8(SI), DX
	LEAQ (R9)(DX*2), DX
	MOVQ 16(SI), CX
	VPXOR Y8, Y8, Y8
	CMPQ CX, $4
	JL horizontal_tail

horizontal_packet:
	VPMOVZXBW (AX), Y0
	VPSHUFB Y9, Y0, Y0
	VPBROADCASTQ (DX), Y1
	VPSHUFB Y7, Y1, Y1
	VPMADDWD Y1, Y0, Y0
	VPADDD Y0, Y8, Y8
	ADDQ $16, AX
	ADDQ $8, DX
	SUBQ $4, CX
	CMPQ CX, $4
	JGE horizontal_packet
	VEXTRACTI128 $1, Y8, X0
	VPADDD X0, X8, X8

horizontal_tail:
	CMPQ CX, $2
	JL horizontal_last
	VPMOVZXBW (AX), X0
	VPSHUFB X9, X0, X0
	VPBROADCASTD (DX), X1
	VPMADDWD X1, X0, X0
	VPADDD X0, X8, X8
	ADDQ $8, AX
	ADDQ $4, DX
	SUBQ $2, CX
horizontal_last:
	TESTQ CX, CX
	JE horizontal_store
horizontal_single:
	VPMOVZXBD (AX), X0
	MOVWQSX (DX), R10
	VMOVD R10, X1
	VPSHUFD $0, X1, X1
	VPMULLD X1, X0, X0
	VPADDD X0, X8, X8

horizontal_store:
	VPADDD X6, X8, X8
	VPSRAD $9, X8, X8
	VPACKSSDW X8, X8, X8
	VMOVQ X8, (DI)
	ADDQ $8, DI
	ADDQ $24, SI
	DECQ R8
	JNZ horizontal_pixel
horizontal_done:
	VZEROUPPER
	RET

// The wrapper supplies only complete eight-channel blocks. Every source load
// is exactly eight int16 values, independent of row alignment or row stride.
TEXT ·verticalAVX2Blocks(SB), NOSPLIT, $0-72
	MOVQ dst_base+0(FP), DI
	MOVQ dst_len+8(FP), R8
	MOVQ rows_base+24(FP), SI
	MOVQ coefficients_base+48(FP), R9
	MOVQ coefficients_len+56(FP), R10
	XORQ BX, BX
	MOVL $262144, R11
	VMOVD R11, X6
	VPBROADCASTD X6, Y6
	VMOVDQU ·verticalAlphaShuffle(SB), X7
	TESTQ R8, R8
	JE vertical_done

vertical_block:
	MOVQ SI, AX
	MOVQ R9, DX
	MOVQ R10, CX
	VPXOR Y2, Y2, Y2
	CMPQ CX, $2
	JL vertical_tail
vertical_pair:
	MOVQ (AX), R11
	VMOVDQU (R11)(BX*1), X0
	MOVQ 24(AX), R11
	VMOVDQU (R11)(BX*1), X1
	VPUNPCKLWD X1, X0, X3
	VPUNPCKHWD X1, X0, X4
	VINSERTI128 $1, X4, Y3, Y3
	VPBROADCASTD (DX), Y1
	VPMADDWD Y1, Y3, Y3
	VPADDD Y3, Y2, Y2
	ADDQ $48, AX
	ADDQ $4, DX
	SUBQ $2, CX
	CMPQ CX, $2
	JGE vertical_pair
vertical_tail:
	TESTQ CX, CX
	JE vertical_store
	MOVQ (AX), R11
	VPMOVSXWD (R11)(BX*1), Y0
	MOVWQSX (DX), R11
	VMOVD R11, X1
	VPBROADCASTD X1, Y1
	VPMULLD Y1, Y0, Y0
	VPADDD Y0, Y2, Y2

vertical_store:
	VPADDD Y6, Y2, Y2
	VPSRAD $19, Y2, Y2
	VPACKSSDW Y2, Y2, Y0
	VPERMQ $0xd8, Y0, Y0
	VPACKUSWB X0, X0, X0
	VPSHUFB X7, X0, X1
	VPMINUB X1, X0, X0
	VMOVQ X0, (DI)
	ADDQ $8, DI
	ADDQ $16, BX
	SUBQ $8, R8
	JNZ vertical_block
vertical_done:
	VZEROUPPER
	RET

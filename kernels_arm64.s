//go:build arm64 && !purego

#include "textflag.h"

// Go's assembler does not expose the widening signed SIMD multiply or
// saturating narrowing mnemonics. WORD instructions below use the encodings
// in Arm DDI 0602 (Advanced SIMD SMLAL, SSHR, SHRN, SQXTN and SQXTUN).
// Operand order in their comments is Arm order: destination, source(s).
// Go vector load/store/duplicate syntax follows cmd/asm's arm64.s testdata.

// func horizontalNEON(dst []int16, src []byte, spans []span, coefficients []int16)
TEXT ·horizontalNEON(SB), NOSPLIT, $0-96
	MOVD dst_base+0(FP), R0
	MOVD src_base+24(FP), R1
	MOVD spans_base+48(FP), R2
	MOVD spans_len+56(FP), R3
	MOVD coefficients_base+72(FP), R4
	MOVD $256, R10
	CBZ R3, horizontal_done
horizontal_pixel:
	MOVD 0(R2), R5
	MOVD 8(R2), R6
	MOVD 16(R2), R7
	ADD $24, R2
	ADD R5<<2, R1, R8
	ADD R6<<1, R4, R9
	VDUP R10, V0.S4
	CBZ R7, horizontal_store
horizontal_tap:
	// Exactly one RGBA pixel, then unsigned byte -> halfword expansion.
	VLD1.P 4(R8), V1.S[0]
	VUXTL V1.B8, V1.H8
	VLD1R.P 2(R9), [V2.H4]
	WORD $0x0e628020 // SMLAL V0.4S, V1.4H, V2.4H
	SUBS $1, R7
	BNE horizontal_tap
horizontal_store:
	// SHRN discards high bits only after signed rounding. The planner's
	// bounds guarantee the shifted result fits int16, including ringing.
	WORD $0x0f178400 // SHRN V0.4H, V0.4S, #9
	VST1.P [V0.H4], 8(R0)
	SUBS $1, R3
	BNE horizontal_pixel
horizontal_done:
	RET

// func verticalNEONPackets(dst []byte, rows [][]int16, coefficients []int16)
// Four RGBA pixels per packet. R6 is a byte offset into each int16 row.
TEXT ·verticalNEONPackets(SB), NOSPLIT, $0-72
	MOVD dst_base+0(FP), R0
	MOVD dst_len+8(FP), R1
	MOVD rows_base+24(FP), R2
	MOVD rows_len+32(FP), R3
	MOVD coefficients_base+48(FP), R4
	MOVD $262144, R5
	VDUP R5, V7.S4
	// Alpha byte lookup indexes: 3,3,3,3,7,7,7,7,11,...,15.
	VMOVQ $0x0707070703030303, $0x0f0f0f0f0b0b0b0b, V8
	MOVD $0, R6
	CBZ R1, vertical_done
vertical_packet:
	VMOV V7.B16, V0.B16
	VMOV V7.B16, V1.B16
	VMOV V7.B16, V2.B16
	VMOV V7.B16, V3.B16
	MOVD R2, R7
	MOVD R4, R8
	MOVD R3, R9
	CBZ R9, vertical_store
vertical_tap:
	MOVD (R7), R10
	ADD $24, R7
	ADD R6, R10
	VLD1 (R10), [V4.H8, V5.H8]
	VLD1R.P 2(R8), [V6.H8]
	WORD $0x0e668080 // SMLAL  V0.4S, V4.4H, V6.4H
	WORD $0x4e668081 // SMLAL2 V1.4S, V4.8H, V6.8H
	WORD $0x0e6680a2 // SMLAL  V2.4S, V5.4H, V6.4H
	WORD $0x4e6680a3 // SMLAL2 V3.4S, V5.8H, V6.8H
	SUBS $1, R9
	BNE vertical_tap
vertical_store:
	WORD $0x4f2d0400 // SSHR V0.4S, V0.4S, #19
	WORD $0x4f2d0421 // SSHR V1.4S, V1.4S, #19
	WORD $0x4f2d0442 // SSHR V2.4S, V2.4S, #19
	WORD $0x4f2d0463 // SSHR V3.4S, V3.4S, #19
	WORD $0x0e614800 // SQXTN  V0.4H, V0.4S
	WORD $0x4e614820 // SQXTN2 V0.8H, V1.4S
	WORD $0x0e614842 // SQXTN  V2.4H, V2.4S
	WORD $0x4e614862 // SQXTN2 V2.8H, V3.4S
	WORD $0x2e212800 // SQXTUN  V0.8B, V0.8H
	WORD $0x6e212840 // SQXTUN2 V0.16B, V2.8H
	VTBL V8.B16, [V0.B16], V9.B16
	VUMIN V9.B16, V0.B16, V0.B16
	VST1.P [V0.B16], 16(R0)
	ADD $32, R6
	SUBS $16, R1
	BNE vertical_packet
vertical_done:
	RET

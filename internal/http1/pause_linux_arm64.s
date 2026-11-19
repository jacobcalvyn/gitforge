//go:build linux && arm64

#include "textflag.h"

// func cpuPauseN(n int)
TEXT ·cpuPauseN(SB), NOSPLIT|NOFRAME, $0-8
	MOVD	n+0(FP), R0
	CMP	$0, R0
	BLE	done
again:
	YIELD
	SUB	$1, R0
	CBNZ	R0, again
done:
	RET

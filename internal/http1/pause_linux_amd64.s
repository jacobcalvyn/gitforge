//go:build linux && amd64

#include "textflag.h"

// func cpuPauseN(n int)
TEXT ·cpuPauseN(SB), NOSPLIT, $0-8
	MOVQ n+0(FP), CX
	CMPQ CX, $0
	JLE  done
loop:
	PAUSE
	DECQ CX
	JNZ  loop
done:
	RET

//go:build linux && amd64

package http1

import (
	"syscall"
	"unsafe"
)

const archSpinScale = 1

func rawEpollWait0(epfd int, events []syscall.EpollEvent) (int, syscall.Errno) {
	r1, _, errno := syscall.RawSyscall6(syscall.SYS_EPOLL_WAIT, uintptr(epfd), uintptr(unsafe.Pointer(&events[0])), uintptr(len(events)), 0, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r1), 0
}

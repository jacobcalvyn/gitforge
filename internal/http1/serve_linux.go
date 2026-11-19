//go:build linux && amd64

package http1

import (
	"errors"
	"net"
	"sync"
	"syscall"
)

type epollConn struct {
	fd              int
	epfd            int
	state           connState
	outPending      []byte
	outOff          int
	epollOut        bool
	closeAfterWrite bool
	writeErr        bool
}

func (c *epollConn) armOut() {
	if c.epollOut {
		return
	}
	var ev syscall.EpollEvent
	ev.Events = syscall.EPOLLIN | syscall.EPOLLOUT | syscall.EPOLLRDHUP
	ev.Fd = int32(c.fd)
	if err := syscall.EpollCtl(c.epfd, syscall.EPOLL_CTL_MOD, c.fd, &ev); err == nil {
		c.epollOut = true
	} else {
		c.writeErr = true
	}
}

func (c *epollConn) disarmOut() {
	if !c.epollOut {
		return
	}
	var ev syscall.EpollEvent
	ev.Events = syscall.EPOLLIN | syscall.EPOLLRDHUP
	ev.Fd = int32(c.fd)
	if err := syscall.EpollCtl(c.epfd, syscall.EPOLL_CTL_MOD, c.fd, &ev); err == nil {
		c.epollOut = false
	} else {
		c.writeErr = true
	}
}

func (c *epollConn) writeBytes(data []byte) error {
	if c.writeErr {
		return errors.New("connection write failed")
	}
	if len(c.outPending) == 0 {
		for len(data) > 0 {
			n, err := syscall.Write(c.fd, data)
			if n > 0 {
				data = data[n:]
				continue
			}
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			c.writeErr = true
			return err
		}
		if len(data) == 0 {
			return nil
		}
	}
	c.outPending = append(c.outPending, data...)
	c.armOut()
	return nil
}

func (c *epollConn) writeBuffers(hdr, body []byte) error {
	if err := c.writeBytes(hdr); err != nil {
		return err
	}
	return c.writeBytes(body)
}

func (c *epollConn) flushPending() bool {
	for c.outOff < len(c.outPending) {
		n, err := syscall.Write(c.fd, c.outPending[c.outOff:])
		if n > 0 {
			c.outOff += n
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			return true
		}
		c.writeErr = true
		return false
	}
	c.outPending = c.outPending[:0]
	c.outOff = 0
	if c.closeAfterWrite {
		return false
	}
	c.disarmOut()
	return !c.writeErr
}

var epollConnPool = sync.Pool{
	New: func() any {
		return &epollConn{
			outPending: make([]byte, 0, 8192),
		}
	},
}

// Serve accepts and processes HTTP/1.1 connections using a single-threaded Linux epoll event loop.
func (s *Server) Serve(ln net.Listener) error {
	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		return s.serveBlocking(ln)
	}
	rawConn, err := tcpLn.SyscallConn()
	if err != nil {
		return s.serveBlocking(ln)
	}
	dupFD := -1
	var dupErr error
	if err := rawConn.Control(func(fd uintptr) {
		dupFD, dupErr = syscall.Dup(int(fd))
	}); err != nil {
		return err
	}
	if dupErr != nil {
		return dupErr
	}
	_ = syscall.SetNonblock(dupFD, true)
	syscall.CloseOnExec(dupFD)

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		_ = syscall.Close(dupFD)
		return s.serveBlocking(ln)
	}

	var wakePipe [2]int
	if err := syscall.Pipe2(wakePipe[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		_ = syscall.Close(epfd)
		_ = syscall.Close(dupFD)
		return s.serveBlocking(ln)
	}
	wakeRFD := wakePipe[0]
	wakeWFD := wakePipe[1]

	s.epollMu.Lock()
	if s.closed.Load() {
		s.epollMu.Unlock()
		_ = syscall.Close(wakeRFD)
		_ = syscall.Close(wakeWFD)
		_ = syscall.Close(epfd)
		_ = syscall.Close(dupFD)
		return nil
	}
	s.wakeWFD = wakeWFD
	s.hasWake = true
	s.epollMu.Unlock()

	var lEv syscall.EpollEvent
	lEv.Events = syscall.EPOLLIN
	lEv.Fd = int32(dupFD)
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, dupFD, &lEv); err != nil {
		s.cleanupWake(wakeRFD, wakeWFD)
		_ = syscall.Close(epfd)
		_ = syscall.Close(dupFD)
		return err
	}

	var wEv syscall.EpollEvent
	wEv.Events = syscall.EPOLLIN
	wEv.Fd = int32(wakeRFD)
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, wakeRFD, &wEv); err != nil {
		s.cleanupWake(wakeRFD, wakeWFD)
		_ = syscall.Close(epfd)
		_ = syscall.Close(dupFD)
		return err
	}

	conns := make([]*epollConn, 256)
	putConn := func(fd int, c *epollConn) {
		if fd >= len(conns) {
			nextLen := fd + 128
			if nextLen < len(conns)*2 {
				nextLen = len(conns) * 2
			}
			nc := make([]*epollConn, nextLen)
			copy(nc, conns)
			conns = nc
		}
		conns[fd] = c
	}
	closeConn := func(fd int) {
		if fd < 0 || fd >= len(conns) {
			return
		}
		c := conns[fd]
		if c == nil {
			return
		}
		conns[fd] = nil
		_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, fd, nil)
		_ = syscall.Close(fd)
		c.fd = -1
		c.epfd = -1
		c.outPending = c.outPending[:0]
		c.outOff = 0
		c.epollOut = false
		c.closeAfterWrite = false
		c.writeErr = false
		c.state.filled = 0
		c.state.resetPerReq()
		epollConnPool.Put(c)
	}
	shutdownAll := func() {
		for fd, c := range conns {
			if c != nil {
				closeConn(fd)
			}
		}
		s.cleanupWake(wakeRFD, wakeWFD)
		_ = syscall.Close(epfd)
		_ = syscall.Close(dupFD)
	}

	var events [128]syscall.EpollEvent
	for {
		n, err := syscall.EpollWait(epfd, events[:], -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			shutdownAll()
			if s.closed.Load() {
				return nil
			}
			return err
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			ev := events[i].Events

			if fd == wakeRFD || s.closed.Load() {
				shutdownAll()
				return nil
			}

			if fd == dupFD {
				for {
					nfd, _, err := syscall.Accept4(dupFD, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
					if err != nil {
						if err == syscall.EINTR {
							continue
						}
						if err == syscall.ECONNABORTED {
							continue
						}
						if err == syscall.ENOSYS {
							nfd, _, err = syscall.Accept(dupFD)
							if err != nil {
								break
							}
							_ = syscall.SetNonblock(nfd, true)
							syscall.CloseOnExec(nfd)
						} else {
							break
						}
					}
					_ = syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
					c := epollConnPool.Get().(*epollConn)
					c.fd = nfd
					c.epfd = epfd
					c.outPending = c.outPending[:0]
					c.outOff = 0
					c.epollOut = false
					c.closeAfterWrite = false
					c.writeErr = false
					c.state.init(c)
					putConn(nfd, c)

					var cev syscall.EpollEvent
					cev.Events = syscall.EPOLLIN | syscall.EPOLLRDHUP
					cev.Fd = int32(nfd)
					if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, nfd, &cev); err != nil {
						closeConn(nfd)
					}
				}
				continue
			}

			if fd < 0 || fd >= len(conns) {
				continue
			}
			c := conns[fd]
			if c == nil {
				continue
			}

			if ev&syscall.EPOLLOUT != 0 {
				if !c.flushPending() {
					closeConn(fd)
					continue
				}
			}

			if ev&(syscall.EPOLLIN|syscall.EPOLLERR|syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0 {
				peerClosed := false
				readFatal := false
				rdhup := ev&(syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0

				for {
					if c.state.filled == len(c.state.readBuf) {
						if len(c.state.readBuf) >= maxHeaderBytes+maxBodyBytes {
							readFatal = true
							break
						}
						c.state.ensureReadCap(len(c.state.readBuf) * 2)
					}
					avail := len(c.state.readBuf) - c.state.filled
					nr, rerr := syscall.Read(fd, c.state.readBuf[c.state.filled:])
					if nr > 0 {
						c.state.filled += nr
						if !s.processReadBuffer(&c.state) {
							c.closeAfterWrite = true
							break
						}
						if c.writeErr {
							readFatal = true
							break
						}
						if nr < avail && !rdhup {
							break
						}
						continue
					}
					if nr == 0 {
						peerClosed = true
						break
					}
					if rerr == syscall.EINTR {
						continue
					}
					if rerr == syscall.EAGAIN || rerr == syscall.EWOULDBLOCK {
						break
					}
					readFatal = true
					break
				}

				if readFatal || c.writeErr {
					closeConn(fd)
					continue
				}
				if peerClosed || c.closeAfterWrite || (ev&syscall.EPOLLERR != 0) {
					if len(c.outPending) == 0 {
						closeConn(fd)
					} else {
						c.closeAfterWrite = true
						c.armOut()
					}
				}
			}
		}
	}
}

func (s *Server) cleanupWake(wakeRFD, wakeWFD int) {
	s.epollMu.Lock()
	s.hasWake = false
	s.wakeWFD = -1
	s.epollMu.Unlock()
	_ = syscall.Close(wakeRFD)
	_ = syscall.Close(wakeWFD)
}

// Close stops accepting new connections and wakes the epoll event loop.
func (s *Server) Close(ln net.Listener) {
	s.closed.Store(true)
	if ln != nil {
		_ = ln.Close()
	}
	s.epollMu.Lock()
	if s.hasWake && s.wakeWFD >= 0 {
		var b [1]byte
		b[0] = 1
		_, _ = syscall.Write(s.wakeWFD, b[:])
	}
	s.epollMu.Unlock()
}

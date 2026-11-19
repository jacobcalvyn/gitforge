//go:build !(linux && (amd64 || arm64))

package http1

import "net"

// Serve accepts connections and processes HTTP/1.1 requests using the shared FSM.
func (s *Server) Serve(ln net.Listener) error {
	return s.serveBlocking(ln)
}

// Close marks the server closed and closes the listener.
func (s *Server) Close(ln net.Listener) {
	s.closed.Store(true)
	if ln != nil {
		_ = ln.Close()
	}
}

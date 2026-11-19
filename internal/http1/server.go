package http1

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	maxHeaderBytes = 64 * 1024
	maxBodyBytes   = 64 * 1024 * 1024
	initReadBuf    = 64 * 1024
	initWriteBuf   = 64 * 1024
	idleTimeout    = 120 * time.Second
)

// Header represents a single HTTP header pair.
type Header struct {
	Name  string
	Value string
}

// Request represents a parsed HTTP/1.x request.
type Request struct {
	Method             string
	RawTarget          string
	Path               string
	RawQuery           string
	Proto              string
	Close              bool
	Host               string
	Auth               string
	ContentType        string
	GitProtocol        string
	MCPSessionID       string
	MCPProtocolVersion string
	Headers            []Header
	Body               []byte
}

// QueryParam returns the decoded value of query parameter key, or "" if absent.
func (r *Request) QueryParam(key string) string {
	q := r.RawQuery
	for len(q) > 0 {
		var part string
		if idx := strings.IndexByte(q, '&'); idx >= 0 {
			part = q[:idx]
			q = q[idx+1:]
		} else {
			part = q
			q = ""
		}
		if part == "" {
			continue
		}
		k := part
		v := ""
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			k = part[:eq]
			v = part[eq+1:]
		}
		if k == key {
			if !strings.ContainsAny(v, "%+") {
				return v
			}
			decoded, err := url.QueryUnescape(v)
			if err != nil {
				return v
			}
			return decoded
		}
		if strings.ContainsAny(k, "%+") {
			if dk, err := url.QueryUnescape(k); err == nil && dk == key {
				if !strings.ContainsAny(v, "%+") {
					return v
				}
				if dv, err := url.QueryUnescape(v); err == nil {
					return dv
				}
				return v
			}
		}
	}
	return ""
}

// HeaderValue returns the first value matching name (case-insensitive), or "".
func (r *Request) HeaderValue(name string) string {
	switch {
	case strings.EqualFold(name, "Host"):
		return r.Host
	case strings.EqualFold(name, "Authorization"):
		return r.Auth
	case strings.EqualFold(name, "Content-Type"):
		return r.ContentType
	case strings.EqualFold(name, "Git-Protocol"):
		return r.GitProtocol
	case strings.EqualFold(name, "Mcp-Session-Id"):
		return r.MCPSessionID
	case strings.EqualFold(name, "Mcp-Protocol-Version"):
		return r.MCPProtocolVersion
	}
	for i := range r.Headers {
		if strings.EqualFold(r.Headers[i].Name, name) {
			return r.Headers[i].Value
		}
	}
	return ""
}

// ResponseWriter writes HTTP/1.1 responses in a single syscall whenever possible.
type ResponseWriter struct {
	conn  net.Conn
	buf   []byte
	reqID *atomic.Uint64
	wrote bool
}

func b2s(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

// StatusText returns the standard HTTP reason phrase for status codes we emit.
func StatusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 413:
		return "Payload Too Large"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "OK"
	}
}

// BuildResponse constructs a complete HTTP/1.1 response byte slice suitable for
// atomic caching and single-syscall conn.Write().
func BuildResponse(status int, contentType string, extraHeaders []Header, requestID string, body []byte) []byte {
	var b []byte
	// Estimate capacity to avoid reallocations.
	est := 192 + len(contentType) + len(requestID) + len(body) + len(extraHeaders)*48
	b = make([]byte, 0, est)
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	b = append(b, "\r\nConnection: keep-alive\r\n"...)
	if requestID != "" {
		b = append(b, "X-GitHub-Request-Id: "...)
		b = append(b, requestID...)
		b = append(b, "\r\n"...)
	}
	if contentType != "" {
		b = append(b, "Content-Type: "...)
		b = append(b, contentType...)
		b = append(b, "\r\n"...)
	}
	for _, h := range extraHeaders {
		b = append(b, h.Name...)
		b = append(b, ": "...)
		b = append(b, h.Value...)
		b = append(b, "\r\n"...)
	}
	if status != 204 && status != 304 {
		b = append(b, "Content-Length: "...)
		b = strconv.AppendInt(b, int64(len(body)), 10)
		b = append(b, "\r\n"...)
	}
	b = append(b, "\r\n"...)
	if status != 204 && status != 304 && len(body) > 0 {
		b = append(b, body...)
	}
	return b
}

// WriteRaw sends a pre-built HTTP/1.1 response directly in a single Write call.
func (w *ResponseWriter) WriteRaw(raw []byte) error {
	w.wrote = true
	return writeAll(w.conn, raw)
}

// WriteResponse serializes headers + body and emits them with minimal syscalls.
func (w *ResponseWriter) WriteResponse(status int, contentType string, extraHeaders []Header, body []byte) error {
	w.wrote = true
	id := w.reqID.Add(1)

	b := w.buf[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	b = append(b, "\r\nConnection: keep-alive\r\nX-GitHub-Request-Id: req-"...)
	b = strconv.AppendUint(b, id, 10)
	b = append(b, "\r\n"...)
	if contentType != "" {
		b = append(b, "Content-Type: "...)
		b = append(b, contentType...)
		b = append(b, "\r\n"...)
	}
	for _, h := range extraHeaders {
		b = append(b, h.Name...)
		b = append(b, ": "...)
		b = append(b, h.Value...)
		b = append(b, "\r\n"...)
	}
	if status != 204 && status != 304 {
		b = append(b, "Content-Length: "...)
		b = strconv.AppendInt(b, int64(len(body)), 10)
		b = append(b, "\r\n"...)
	}
	b = append(b, "\r\n"...)

	if status == 204 || status == 304 || len(body) == 0 {
		w.buf = b
		return writeAll(w.conn, b)
	}

	// Copy small bodies into the reusable write buffer for a single write syscall;
	// use writev (net.Buffers) for large bodies to avoid extra copies.
	if len(body) <= 256*1024 {
		b = append(b, body...)
		w.buf = b
		return writeAll(w.conn, b)
	}
	w.buf = b
	bufs := net.Buffers{b, body}
	_, err := bufs.WriteTo(w.conn)
	return err
}

// WriteJSON writes an application/json; charset=utf-8 response.
func (w *ResponseWriter) WriteJSON(status int, body []byte) error {
	return w.WriteResponse(status, "application/json; charset=utf-8", nil, body)
}

// WriteJSONWithHeaders writes an application/json; charset=utf-8 response with custom headers.
func (w *ResponseWriter) WriteJSONWithHeaders(status int, headers []Header, body []byte) error {
	return w.WriteResponse(status, "application/json; charset=utf-8", headers, body)
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Handler processes a parsed HTTP request.
type Handler func(w *ResponseWriter, r *Request)

// Server is a raw-TCP HTTP/1.1 server optimized for low syscall overhead.
type Server struct {
	Handler Handler
	reqID   atomic.Uint64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// Serve accepts connections from ln until ln is closed or Serve stops.
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// Close marks the server closed and waits briefly for active connections.
func (s *Server) Close(ln net.Listener) {
	s.closed.Store(true)
	if ln != nil {
		_ = ln.Close()
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	readBuf := make([]byte, initReadBuf)
	filled := 0
	var bodyScratch []byte

	req := Request{
		Headers: make([]Header, 0, 8),
	}
	w := ResponseWriter{
		conn:  conn,
		buf:   make([]byte, 0, initWriteBuf),
		reqID: &s.reqID,
	}

	lastDeadline := time.Now()
	_ = conn.SetReadDeadline(lastDeadline.Add(idleTimeout))
	var reqCount uint32

	for {
		if s.closed.Load() {
			return
		}
		reqCount++
		if reqCount&0xff == 0 {
			now := time.Now()
			if now.Sub(lastDeadline) > 15*time.Second {
				lastDeadline = now
				_ = conn.SetReadDeadline(now.Add(idleTimeout))
			}
		}

		// Locate end of headers (\r\n\r\n or \n\n).
		hdrEnd := -1
		sepLen := 4
		for {
			if idx := bytes.Index(readBuf[:filled], []byte("\r\n\r\n")); idx >= 0 {
				hdrEnd = idx
				sepLen = 4
				break
			}
			if idx := bytes.Index(readBuf[:filled], []byte("\n\n")); idx >= 0 {
				hdrEnd = idx
				sepLen = 2
				break
			}
			if filled >= maxHeaderBytes {
				_ = w.WriteJSON(400, []byte(`{"message":"Request headers too large"}`+"\n"))
				return
			}
			if filled == len(readBuf) {
				nextSize := len(readBuf) * 2
				if nextSize > maxHeaderBytes+4096 {
					nextSize = maxHeaderBytes + 4096
				}
				if nextSize == len(readBuf) {
					_ = w.WriteJSON(400, []byte(`{"message":"Request headers too large"}`+"\n"))
					return
				}
				nb := make([]byte, nextSize)
				copy(nb, readBuf[:filled])
				readBuf = nb
			}
			n, err := conn.Read(readBuf[filled:])
			if n > 0 {
				filled += n
			}
			if err != nil {
				return
			}
		}

		bodyStart := hdrEnd + sepLen
		hdrBytes := readBuf[:hdrEnd]

		req.Method = ""
		req.RawTarget = ""
		req.Path = ""
		req.RawQuery = ""
		req.Proto = ""
		req.Close = false
		req.Host = ""
		req.Auth = ""
		req.ContentType = ""
		req.GitProtocol = ""
		req.MCPSessionID = ""
		req.MCPProtocolVersion = ""
		req.Headers = req.Headers[:0]
		req.Body = nil

		contentLength := -1
		chunked := false
		expectContinue := false

		// Parse request line.
		lineEnd := bytes.IndexByte(hdrBytes, '\n')
		var reqLine []byte
		var restHdr []byte
		if lineEnd < 0 {
			reqLine = hdrBytes
		} else {
			reqLine = hdrBytes[:lineEnd]
			restHdr = hdrBytes[lineEnd+1:]
		}
		reqLine = bytes.TrimSuffix(reqLine, []byte("\r"))

		sp1 := bytes.IndexByte(reqLine, ' ')
		if sp1 <= 0 {
			_ = w.WriteJSON(400, []byte(`{"message":"Malformed request line"}`+"\n"))
			return
		}
		sp2 := bytes.IndexByte(reqLine[sp1+1:], ' ')
		if sp2 <= 0 {
			_ = w.WriteJSON(400, []byte(`{"message":"Malformed request line"}`+"\n"))
			return
		}
		req.Method = b2s(reqLine[:sp1])
		rawTgt := reqLine[sp1+1 : sp1+1+sp2]
		req.RawTarget = b2s(rawTgt)
		req.Proto = b2s(reqLine[sp1+1+sp2+1:])
		if req.Proto == "HTTP/1.0" {
			req.Close = true
		}

		// Split target into path and query without allocations on common plain paths.
		if bytes.HasPrefix(rawTgt, []byte("http://")) || bytes.HasPrefix(rawTgt, []byte("https://")) {
			if u, err := url.Parse(req.RawTarget); err == nil {
				req.Path = u.Path
				req.RawQuery = u.RawQuery
			} else {
				req.Path = req.RawTarget
			}
		} else {
			if qm := bytes.IndexByte(rawTgt, '?'); qm >= 0 {
				pathPart := rawTgt[:qm]
				if bytes.IndexByte(pathPart, '%') < 0 {
					req.Path = b2s(pathPart)
				} else {
					req.Path = unescapePath(b2s(pathPart))
				}
				req.RawQuery = b2s(rawTgt[qm+1:])
			} else {
				if bytes.IndexByte(rawTgt, '%') < 0 {
					req.Path = b2s(rawTgt)
				} else {
					req.Path = unescapePath(b2s(rawTgt))
				}
			}
		}

		// Parse header lines with zero allocations for standard headers.
		for len(restHdr) > 0 {
			nl := bytes.IndexByte(restHdr, '\n')
			var line []byte
			if nl < 0 {
				line = restHdr
				restHdr = nil
			} else {
				line = restHdr[:nl]
				restHdr = restHdr[nl+1:]
			}
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) == 0 {
				continue
			}
			colon := bytes.IndexByte(line, ':')
			if colon <= 0 {
				_ = w.WriteJSON(400, []byte(`{"message":"Malformed header"}`+"\n"))
				return
			}
			nameBytes := bytes.TrimSpace(line[:colon])
			valBytes := bytes.TrimSpace(line[colon+1:])

			switch {
			case bytesEqualFoldASCII(nameBytes, "Content-Length"):
				cl, err := parsePositiveInt(valBytes)
				if err != nil || cl < 0 || cl > maxBodyBytes {
					_ = w.WriteJSON(400, []byte(`{"message":"Invalid Content-Length"}`+"\n"))
					return
				}
				contentLength = cl
			case bytesEqualFoldASCII(nameBytes, "Transfer-Encoding"):
				if containsFoldASCII(valBytes, "chunked") {
					chunked = true
				}
			case bytesEqualFoldASCII(nameBytes, "Connection"):
				if containsFoldASCII(valBytes, "close") {
					req.Close = true
				} else if containsFoldASCII(valBytes, "keep-alive") {
					req.Close = false
				}
			case bytesEqualFoldASCII(nameBytes, "Host"):
				req.Host = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Authorization"):
				req.Auth = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Content-Type"):
				req.ContentType = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Git-Protocol"):
				req.GitProtocol = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Mcp-Session-Id"):
				req.MCPSessionID = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Mcp-Protocol-Version"):
				req.MCPProtocolVersion = b2s(valBytes)
			case bytesEqualFoldASCII(nameBytes, "Expect"):
				if bytesEqualFoldASCII(valBytes, "100-continue") {
					expectContinue = true
				}
			case bytesEqualFoldASCII(nameBytes, "Accept"),
				bytesEqualFoldASCII(nameBytes, "User-Agent"),
				bytesEqualFoldASCII(nameBytes, "Accept-Encoding"),
				bytesEqualFoldASCII(nameBytes, "Accept-Language"):
				// Ignored on hot path to avoid per-request allocations.
			default:
				req.Headers = append(req.Headers, Header{Name: b2s(nameBytes), Value: b2s(valBytes)})
			}
		}

		if expectContinue {
			if err := writeAll(conn, []byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return
			}
		}

		// Read body.
		consumed := bodyStart
		if chunked {
			var err error
			var body []byte
			body, consumed, readBuf, filled, err = readChunkedBody(conn, readBuf, bodyStart, filled, bodyScratch[:0])
			if err != nil {
				_ = w.WriteJSON(400, []byte(`{"message":"Malformed chunked body"}`+"\n"))
				return
			}
			bodyScratch = body
			req.Body = body
		} else if contentLength > 0 {
			need := bodyStart + contentLength
			if need > len(readBuf) {
				nb := make([]byte, need)
				copy(nb, readBuf[:filled])
				readBuf = nb
			}
			for filled < need {
				n, err := conn.Read(readBuf[filled:need])
				if n > 0 {
					filled += n
				}
				if err != nil {
					if errors.Is(err, io.EOF) && filled == need {
						break
					}
					return
				}
			}
			req.Body = readBuf[bodyStart:need]
			consumed = need
		}

		w.wrote = false
		s.dispatch(&w, &req)

		// Shift leftover pipelined bytes (if any) to front of readBuf.
		rem := filled - consumed
		if rem > 0 {
			copy(readBuf, readBuf[consumed:filled])
		}
		filled = rem

		if req.Close {
			return
		}
	}
}

func (s *Server) dispatch(w *ResponseWriter, r *Request) {
	defer func() {
		if rec := recover(); rec != nil {
			if !w.wrote {
				_ = w.WriteJSON(500, []byte(fmt.Sprintf(`{"message":"Internal server error: %v"}`+"\n", rec)))
			}
		}
	}()
	s.Handler(w, r)
}

func containsFoldASCII(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	limit := len(haystack) - len(needle)
	for i := 0; i <= limit; i++ {
		if bytesEqualFoldASCII(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func bytesEqualFoldASCII(b []byte, target string) bool {
	if len(b) != len(target) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		t := target[i]
		if c == t {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if t >= 'A' && t <= 'Z' {
			t += 'a' - 'A'
		}
		if c != t {
			return false
		}
	}
	return true
}

func parsePositiveInt(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errors.New("empty int")
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid int")
		}
		n = n*10 + int(c-'0')
		if n > maxBodyBytes {
			return 0, errors.New("int too large")
		}
	}
	return n, nil
}

func unescapePath(p string) string {
	if !strings.ContainsRune(p, '%') {
		return p
	}
	u, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return u
}

func readChunkedBody(conn net.Conn, readBuf []byte, pos, filled int, out []byte) ([]byte, int, []byte, int, error) {
	ensureLine := func() (int, error) {
		for {
			if idx := bytes.IndexByte(readBuf[pos:filled], '\n'); idx >= 0 {
				return pos + idx, nil
			}
			if filled == len(readBuf) {
				if pos > 0 {
					copy(readBuf, readBuf[pos:filled])
					filled -= pos
					pos = 0
					continue
				}
				if len(readBuf) >= maxHeaderBytes {
					return 0, errors.New("chunk line too long")
				}
				nb := make([]byte, len(readBuf)*2)
				copy(nb, readBuf[:filled])
				readBuf = nb
			}
			n, err := conn.Read(readBuf[filled:])
			if n > 0 {
				filled += n
			}
			if err != nil {
				return 0, err
			}
		}
	}

	for {
		nl, err := ensureLine()
		if err != nil {
			return nil, 0, readBuf, filled, err
		}
		line := bytes.TrimSpace(readBuf[pos:nl])
		pos = nl + 1
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = bytes.TrimSpace(line[:semi])
		}
		sz, err := strconv.ParseInt(string(line), 16, 64)
		if err != nil || sz < 0 || int64(len(out))+sz > maxBodyBytes {
			return nil, 0, readBuf, filled, errors.New("invalid chunk size")
		}
		if sz == 0 {
			// Consume trailer lines until empty line.
			for {
				tl, err := ensureLine()
				if err != nil {
					return nil, 0, readBuf, filled, err
				}
				tline := bytes.TrimSpace(readBuf[pos:tl])
				pos = tl + 1
				if len(tline) == 0 {
					return out, pos, readBuf, filled, nil
				}
			}
		}
		needBytes := int(sz) + 2 // data + CRLF
		for filled-pos < needBytes {
			if pos > 0 && len(readBuf)-pos < needBytes {
				copy(readBuf, readBuf[pos:filled])
				filled -= pos
				pos = 0
			}
			if len(readBuf) < pos+needBytes {
				nb := make([]byte, pos+needBytes+initReadBuf)
				copy(nb, readBuf[:filled])
				readBuf = nb
			}
			n, err := conn.Read(readBuf[filled:])
			if n > 0 {
				filled += n
			}
			if err != nil {
				return nil, 0, readBuf, filled, err
			}
		}
		out = append(out, readBuf[pos:pos+int(sz)]...)
		pos += int(sz)
		if readBuf[pos] == '\r' && readBuf[pos+1] == '\n' {
			pos += 2
		} else if readBuf[pos] == '\n' {
			pos++
		} else {
			return nil, 0, readBuf, filled, errors.New("missing chunk CRLF")
		}
	}
}

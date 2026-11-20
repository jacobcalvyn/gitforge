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
	initReadBuf    = 16 * 1024
	initWriteBuf   = 8 * 1024
	initBodyBuf    = 4096
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

type writerSink interface {
	writeBytes(data []byte) error
	writeBuffers(hdr, body []byte) error
}

// ResponseWriter writes HTTP/1.1 responses in a single syscall whenever possible.
type ResponseWriter struct {
	sink      writerSink
	buf       []byte
	bodyBuf   []byte
	wrote     bool
	closeConn bool
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
// atomic caching and single-syscall writes.
func BuildResponse(status int, contentType string, extraHeaders []Header, requestID string, body []byte) []byte {
	est := 128 + len(contentType) + len(requestID) + len(body) + len(extraHeaders)*48
	b := make([]byte, 0, est)
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	b = append(b, "\r\nX-GitHub-Request-Id: "...)
	if requestID != "" {
		b = append(b, requestID...)
	} else {
		b = append(b, '1')
	}
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
	if status != 204 && status != 304 && len(body) > 0 {
		b = append(b, body...)
	}
	return b
}

// BuildGitResponse constructs a wire-minimal HTTP/1.1 response byte slice for Smart HTTP Git endpoints.
func BuildGitResponse(status int, contentType string, body []byte) []byte {
	est := 96 + len(contentType) + len(body)
	b := make([]byte, 0, est)
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	b = append(b, "\r\nContent-Type: "...)
	b = append(b, contentType...)
	b = append(b, "\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(len(body)), 10)
	b = append(b, "\r\n\r\n"...)
	b = append(b, body...)
	return b
}

// WriteRaw sends a pre-built HTTP/1.1 response directly in a single write call.
func (w *ResponseWriter) WriteRaw(raw []byte) error {
	w.wrote = true
	return w.sink.writeBytes(raw)
}

// WriteResponse serializes headers + body and emits them with minimal syscalls.
func (w *ResponseWriter) WriteResponse(status int, contentType string, extraHeaders []Header, body []byte) error {
	w.wrote = true

	b := w.buf[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	if w.closeConn {
		b = append(b, "\r\nConnection: close"...)
	}
	b = append(b, "\r\nX-GitHub-Request-Id: 1\r\n"...)
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
		return w.sink.writeBytes(b)
	}

	if len(body) <= 256*1024 {
		b = append(b, body...)
		w.buf = b
		return w.sink.writeBytes(b)
	}
	w.buf = b
	return w.sink.writeBuffers(b, body)
}

// WriteGitResponse writes a wire-minimal Git Smart HTTP response without unused REST headers.
func (w *ResponseWriter) WriteGitResponse(status int, contentType string, body []byte) error {
	w.wrote = true

	b := w.buf[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	if w.closeConn {
		b = append(b, "\r\nConnection: close"...)
	}
	b = append(b, "\r\nContent-Type: "...)
	b = append(b, contentType...)
	b = append(b, "\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(len(body)), 10)
	b = append(b, "\r\n\r\n"...)

	if len(body) == 0 {
		w.buf = b
		return w.sink.writeBytes(b)
	}
	if len(body) <= 256*1024 {
		b = append(b, body...)
		w.buf = b
		return w.sink.writeBytes(b)
	}
	w.buf = b
	return w.sink.writeBuffers(b, body)
}

// WriteJSON writes a wire-minimal application/json response.
func (w *ResponseWriter) WriteJSON(status int, body []byte) error {
	w.wrote = true

	b := w.buf[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = append(b, StatusText(status)...)
	if w.closeConn {
		b = append(b, "\r\nConnection: close"...)
	}
	b = append(b, "\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: 1\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(len(body)), 10)
	b = append(b, "\r\n\r\n"...)

	if len(body) <= 256*1024 {
		b = append(b, body...)
		w.buf = b
		return w.sink.writeBytes(b)
	}
	w.buf = b
	return w.sink.writeBuffers(b, body)
}

// WriteJSONBuild constructs the JSON body into a pooled scratch slice and writes it with zero heap allocations.
func (w *ResponseWriter) WriteJSONBuild(status int, buildBody func(dst []byte) []byte) error {
	w.wrote = true
	w.bodyBuf = buildBody(w.bodyBuf[:0])
	return w.WriteJSON(status, w.bodyBuf)
}

// BeginJSON201 prepares the HTTP/1.1 201 Created response header inside w.buf and
// returns the buffer along with its header end offset for closure-free body appending.
func (w *ResponseWriter) BeginJSON201(bodyLen int) ([]byte, int) {
	w.wrote = true

	b := w.buf[:0]
	b = append(b, "HTTP/1.1 201 Created\r\n"...)
	if w.closeConn {
		b = append(b, "Connection: close\r\n"...)
	}
	b = append(b, "Content-Type: application/json\r\nX-GitHub-Request-Id: 1\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(bodyLen), 10)
	b = append(b, "\r\n\r\n"...)
	return b, len(b)
}

// CommitJSON201 finalizes and writes a 201 Created response prepared by BeginJSON201.
func (w *ResponseWriter) CommitJSON201(b []byte, hdrLen, bodyLen int) error {
	if len(b)-hdrLen != bodyLen {
		return w.WriteJSON(201, b[hdrLen:])
	}
	w.buf = b
	return w.sink.writeBytes(b)
}

// WriteJSON201Sized writes a 201 Created application/json response directly into w.buf in a single pass.
func (w *ResponseWriter) WriteJSON201Sized(bodyLen int, appendBody func(dst []byte) []byte) error {
	b, hdrLen := w.BeginJSON201(bodyLen)
	b = appendBody(b)
	return w.CommitJSON201(b, hdrLen, bodyLen)
}

// WriteJSONWithHeaders writes an application/json response with custom headers.
func (w *ResponseWriter) WriteJSONWithHeaders(status int, headers []Header, body []byte) error {
	return w.WriteResponse(status, "application/json", headers, body)
}

// Handler processes a parsed HTTP request.
type Handler func(w *ResponseWriter, r *Request)

// Server is a raw-TCP HTTP/1.1 server optimized for low syscall and scheduler overhead.
type Server struct {
	Handler     Handler
	ActivitySeq atomic.Uint64

	wg       sync.WaitGroup
	closed   atomic.Bool
	epollMu  sync.Mutex
	wakeWFD  int
	hasWake  bool
}

type connState struct {
	readBuf     []byte
	filled      int
	bodyScratch []byte
	req         Request
	w           ResponseWriter

	hdrParsed      bool
	bodyStart      int
	contentLength  int
	chunked        bool
	expectContinue bool
	sentContinue   bool
}

func (c *connState) init(sink writerSink) {
	if c.readBuf == nil {
		c.readBuf = make([]byte, initReadBuf)
	}
	c.filled = 0
	if c.req.Headers == nil {
		c.req.Headers = make([]Header, 0, 8)
	} else {
		c.req.Headers = c.req.Headers[:0]
	}
	c.w.sink = sink
	if c.w.buf == nil {
		c.w.buf = make([]byte, 0, initWriteBuf)
	} else {
		c.w.buf = c.w.buf[:0]
	}
	if c.w.bodyBuf == nil {
		c.w.bodyBuf = make([]byte, 0, initBodyBuf)
	} else {
		c.w.bodyBuf = c.w.bodyBuf[:0]
	}
	c.w.wrote = false
	c.w.closeConn = false
	c.hdrParsed = false
	c.bodyStart = 0
	c.contentLength = -1
	c.chunked = false
	c.expectContinue = false
	c.sentContinue = false
}

func (c *connState) resetPerReq() {
	c.hdrParsed = false
	c.bodyStart = 0
	c.contentLength = -1
	c.chunked = false
	c.expectContinue = false
	c.sentContinue = false
}

func (c *connState) ensureReadCap(minCap int) {
	if minCap <= len(c.readBuf) {
		return
	}
	nextCap := len(c.readBuf) * 2
	if nextCap < minCap {
		nextCap = minCap
	}
	nb := make([]byte, nextCap)
	copy(nb, c.readBuf[:c.filled])
	c.readBuf = nb
}

var (
	crlfcrlf = []byte("\r\n\r\n")
	lflf     = []byte("\n\n")
)

func findHeaderEnd(buf []byte) (hdrEnd, sepLen int) {
	if idx4 := bytes.Index(buf, crlfcrlf); idx4 >= 0 {
		nl := bytes.IndexByte(buf[:idx4], '\n')
		if nl < 0 || (nl > 0 && buf[nl-1] == '\r') {
			return idx4, 4
		}
		if idx2 := bytes.Index(buf[:idx4], lflf); idx2 >= 0 {
			return idx2, 2
		}
		return idx4, 4
	}
	if idx2 := bytes.Index(buf, lflf); idx2 >= 0 {
		return idx2, 2
	}
	return -1, 0
}

func (s *Server) parseRequestHeaders(c *connState, hdrBytes []byte) bool {
	req := &c.req
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

	c.contentLength = -1
	c.chunked = false
	c.expectContinue = false

	lineEnd := bytes.IndexByte(hdrBytes, '\n')
	var reqLine, restHdr []byte
	if lineEnd < 0 {
		reqLine = hdrBytes
	} else {
		reqLine = hdrBytes[:lineEnd]
		restHdr = hdrBytes[lineEnd+1:]
	}
	if len(reqLine) > 0 && reqLine[len(reqLine)-1] == '\r' {
		reqLine = reqLine[:len(reqLine)-1]
	}

	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		_ = c.w.WriteJSON(400, []byte(`{"message":"Malformed request line"}`+"\n"))
		return false
	}
	sp2 := bytes.IndexByte(reqLine[sp1+1:], ' ')
	if sp2 <= 0 {
		_ = c.w.WriteJSON(400, []byte(`{"message":"Malformed request line"}`+"\n"))
		return false
	}
	req.Method = b2s(reqLine[:sp1])
	rawTgt := reqLine[sp1+1 : sp1+1+sp2]
	req.RawTarget = b2s(rawTgt)
	req.Proto = b2s(reqLine[sp1+1+sp2+1:])
	if req.Proto == "HTTP/1.0" {
		req.Close = true
	}

	if bytes.HasPrefix(rawTgt, []byte("http://")) || bytes.HasPrefix(rawTgt, []byte("https://")) {
		if u, err := url.Parse(req.RawTarget); err == nil {
			req.Path = u.Path
			req.RawQuery = u.RawQuery
		} else {
			req.Path = req.RawTarget
		}
	} else if qm := bytes.IndexByte(rawTgt, '?'); qm >= 0 {
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
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			_ = c.w.WriteJSON(400, []byte(`{"message":"Malformed header"}`+"\n"))
			return false
		}
		nameBytes := line[:colon]
		if len(nameBytes) > 0 && (isASCIIWS(nameBytes[0]) || isASCIIWS(nameBytes[len(nameBytes)-1])) {
			nameBytes = trimASCIIWhitespace(nameBytes)
		}
		valRaw := line[colon+1:]
		if len(valRaw) > 0 && valRaw[0] == ' ' {
			valRaw = valRaw[1:]
		}
		valBytes := valRaw
		if len(valBytes) > 0 && (isASCIIWS(valBytes[0]) || isASCIIWS(valBytes[len(valBytes)-1])) {
			valBytes = trimASCIIWhitespace(valBytes)
		}

		if len(nameBytes) > 0 {
			c0 := nameBytes[0] | 0x20
			switch len(nameBytes) {
			case 4:
				if c0 == 'h' && bytesEqualFoldASCII(nameBytes, "Host") {
					req.Host = b2s(valBytes)
				}
			case 6:
				if c0 == 'e' && bytesEqualFoldASCII(nameBytes, "Expect") && bytesEqualFoldASCII(valBytes, "100-continue") {
					c.expectContinue = true
				}
			case 10:
				if c0 == 'c' && bytesEqualFoldASCII(nameBytes, "Connection") {
					if containsFoldASCII(valBytes, "close") {
						req.Close = true
					} else if containsFoldASCII(valBytes, "keep-alive") {
						req.Close = false
					}
				}
			case 12:
				if c0 == 'c' && bytesEqualFoldASCII(nameBytes, "Content-Type") {
					req.ContentType = b2s(valBytes)
				} else if c0 == 'g' && bytesEqualFoldASCII(nameBytes, "Git-Protocol") {
					req.GitProtocol = b2s(valBytes)
				}
			case 13:
				if c0 == 'a' && bytesEqualFoldASCII(nameBytes, "Authorization") {
					req.Auth = b2s(valBytes)
				}
			case 14:
				if c0 == 'c' && bytesEqualFoldASCII(nameBytes, "Content-Length") {
					cl, err := parsePositiveInt(valBytes)
					if err != nil || cl < 0 || cl > maxBodyBytes {
						_ = c.w.WriteJSON(400, []byte(`{"message":"Invalid Content-Length"}`+"\n"))
						return false
					}
					c.contentLength = cl
				} else if c0 == 'm' && bytesEqualFoldASCII(nameBytes, "Mcp-Session-Id") {
					req.MCPSessionID = b2s(valBytes)
				}
			case 17:
				if c0 == 't' && bytesEqualFoldASCII(nameBytes, "Transfer-Encoding") && containsFoldASCII(valBytes, "chunked") {
					c.chunked = true
				}
			case 20:
				if c0 == 'm' && bytesEqualFoldASCII(nameBytes, "Mcp-Protocol-Version") {
					req.MCPProtocolVersion = b2s(valBytes)
				}
			default:
				// Unused headers are dropped without heap allocations.
			}
		}
	}

	c.hdrParsed = true
	return true
}

type chunkStatus uint8

const (
	chunkNeedMore chunkStatus = iota
	chunkComplete
	chunkBad
)

func tryDecodeChunked(buf []byte, out []byte) ([]byte, int, chunkStatus) {
	pos := 0
	for {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			return nil, 0, chunkNeedMore
		}
		line := trimASCIIWhitespace(buf[pos : pos+nl])
		pos += nl + 1
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = trimASCIIWhitespace(line[:semi])
		}
		sz, ok := parseHexChunkSize(line)
		if !ok || sz < 0 || int64(len(out))+sz > maxBodyBytes {
			return nil, 0, chunkBad
		}
		if sz == 0 {
			for {
				tl := bytes.IndexByte(buf[pos:], '\n')
				if tl < 0 {
					return nil, 0, chunkNeedMore
				}
				tline := trimASCIIWhitespace(buf[pos : pos+tl])
				pos += tl + 1
				if len(tline) == 0 {
					return out, pos, chunkComplete
				}
			}
		}
		dataEnd := pos + int(sz)
		if len(buf) <= dataEnd {
			return nil, 0, chunkNeedMore
		}
		var nextPos int
		if buf[dataEnd] == '\r' {
			if len(buf) < dataEnd+2 {
				return nil, 0, chunkNeedMore
			}
			if buf[dataEnd+1] != '\n' {
				return nil, 0, chunkBad
			}
			nextPos = dataEnd + 2
		} else if buf[dataEnd] == '\n' {
			nextPos = dataEnd + 1
		} else {
			return nil, 0, chunkBad
		}
		out = append(out, buf[pos:dataEnd]...)
		pos = nextPos
	}
}

func (s *Server) processReadBuffer(c *connState) (keepOpen bool) {
	for c.filled > 0 {
		if !c.hdrParsed {
			hdrEnd, sepLen := findHeaderEnd(c.readBuf[:c.filled])
			if hdrEnd < 0 {
				if c.filled >= maxHeaderBytes {
					_ = c.w.WriteJSON(400, []byte(`{"message":"Request headers too large"}`+"\n"))
					return false
				}
				if c.filled == len(c.readBuf) {
					c.ensureReadCap(maxHeaderBytes + 4096)
				}
				return true
			}
			if hdrEnd > maxHeaderBytes {
				_ = c.w.WriteJSON(400, []byte(`{"message":"Request headers too large"}`+"\n"))
				return false
			}
			c.bodyStart = hdrEnd + sepLen
			if !s.parseRequestHeaders(c, c.readBuf[:hdrEnd]) {
				return false
			}
		}

		if c.expectContinue && !c.sentContinue {
			if err := c.w.sink.writeBytes([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return false
			}
			c.sentContinue = true
		}

		consumed := c.bodyStart
		if c.chunked {
			decoded, bodyConsumed, status := tryDecodeChunked(c.readBuf[c.bodyStart:c.filled], c.bodyScratch[:0])
			switch status {
			case chunkNeedMore:
				if c.filled == len(c.readBuf) {
					if len(c.readBuf) >= maxHeaderBytes+maxBodyBytes {
						_ = c.w.WriteJSON(413, []byte(`{"message":"Payload Too Large"}`+"\n"))
						return false
					}
					c.ensureReadCap(len(c.readBuf) * 2)
				}
				return true
			case chunkBad:
				_ = c.w.WriteJSON(400, []byte(`{"message":"Malformed chunked body"}`+"\n"))
				return false
			}
			c.bodyScratch = decoded
			c.req.Body = decoded
			consumed = c.bodyStart + bodyConsumed
		} else if c.contentLength > 0 {
			need := c.bodyStart + c.contentLength
			if c.filled < need {
				c.ensureReadCap(need)
				return true
			}
			c.req.Body = c.readBuf[c.bodyStart:need]
			consumed = need
		}

		s.ActivitySeq.Add(1)
		c.w.wrote = false
		c.w.closeConn = c.req.Close
		s.dispatch(&c.w, &c.req)

		rem := c.filled - consumed
		if rem > 0 {
			copy(c.readBuf, c.readBuf[consumed:c.filled])
		}
		c.filled = rem
		closeAfter := c.req.Close
		c.resetPerReq()
		if closeAfter {
			return false
		}
	}
	return true
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

func isASCIIWS(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func trimASCIIWhitespace(b []byte) []byte {
	start := 0
	for start < len(b) {
		c := b[start]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		start++
	}
	end := len(b)
	for end > start {
		c := b[end-1]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		end--
	}
	return b[start:end]
}

func parseHexChunkSize(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 15 {
		return 0, false
	}
	var val int64
	for _, c := range b {
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			return 0, false
		}
		val = (val << 4) | d
	}
	return val, true
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

type blockingSink struct {
	conn net.Conn
}

func (b *blockingSink) writeBytes(data []byte) error {
	for len(data) > 0 {
		n, err := b.conn.Write(data)
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

func (b *blockingSink) writeBuffers(hdr, body []byte) error {
	bufs := net.Buffers{hdr, body}
	_, err := bufs.WriteTo(b.conn)
	return err
}

func (s *Server) serveBlocking(ln net.Listener) error {
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
		go s.serveBlockingConn(conn)
	}
}

func (s *Server) serveBlockingConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	sink := blockingSink{conn: conn}
	var c connState
	c.init(&sink)

	lastDeadline := time.Now()
	_ = conn.SetReadDeadline(lastDeadline.Add(idleTimeout))
	var readCount uint32

	for {
		if s.closed.Load() {
			return
		}
		readCount++
		if readCount&0xff == 0 {
			now := time.Now()
			if now.Sub(lastDeadline) > 15*time.Second {
				lastDeadline = now
				_ = conn.SetReadDeadline(now.Add(idleTimeout))
			}
		}

		if c.filled == len(c.readBuf) {
			c.ensureReadCap(len(c.readBuf) * 2)
		}
		n, err := conn.Read(c.readBuf[c.filled:])
		if n > 0 {
			c.filled += n
			if !s.processReadBuffer(&c) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

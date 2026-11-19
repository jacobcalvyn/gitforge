package http1

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestServerKeepAliveChunkedAndQuery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Handler: func(w *ResponseWriter, r *Request) {
			if r.Path == "/healthz" {
				_ = w.WriteJSON(200, []byte(`{"ok":true}`+"\n"))
				return
			}
			if r.Path == "/echo" {
				q := r.QueryParam("foo")
				resp := append([]byte(q+"|"), r.Body...)
				_ = w.WriteResponse(201, "text/plain; charset=utf-8", []Header{{Name: "X-Custom", Value: "1"}}, resp)
				return
			}
			_ = w.WriteJSON(404, []byte(`{"message":"Not Found"}`+"\n"))
		},
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close(ln)

	base := "http://" + ln.Addr().String()
	client := &http.Client{}

	// 1. Simple GET /healthz
	r1, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 || strings.TrimSpace(string(b1)) != `{"ok":true}` {
		t.Fatalf("unexpected /healthz: %d %s", r1.StatusCode, b1)
	}
	if r1.Header.Get("X-GitHub-Request-Id") == "" {
		t.Fatal("missing X-GitHub-Request-Id")
	}

	// 2. Raw chunked POST with Expect: 100-continue on keep-alive socket
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reqBytes := "POST /echo?foo=bar%20baz HTTP/1.1\r\nHost: localhost\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
	if _, err := conn.Write([]byte(reqBytes)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	contLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(contLine, "100 Continue") {
		t.Fatalf("expected 100 Continue, got %q (%v)", contLine, err)
	}
	_, _ = br.ReadString('\n') // blank line after 100 Continue

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || string(b2) != "bar baz|hello world" {
		t.Fatalf("unexpected chunked echo: %d %q", resp.StatusCode, b2)
	}

	// 3. Reuse same keep-alive socket for 404
	if _, err := conn.Write([]byte("GET /missing HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp3, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 404 || !bytes.Contains(b3, []byte("Not Found")) {
		t.Fatalf("unexpected 404: %d %q", resp3.StatusCode, b3)
	}
	if srv.ActivitySeq.Load() < 3 {
		t.Fatalf("expected ActivitySeq >= 3, got %d", srv.ActivitySeq.Load())
	}
}

type memSink struct {
	out bytes.Buffer
}

func (m *memSink) writeBytes(data []byte) error {
	_, err := m.out.Write(data)
	return err
}

func (m *memSink) writeBuffers(hdr, body []byte) error {
	_, _ = m.out.Write(hdr)
	_, err := m.out.Write(body)
	return err
}

func TestFSMFragmentedAndPipelined(t *testing.T) {
	srv := &Server{
		Handler: func(w *ResponseWriter, r *Request) {
			switch r.Path {
			case "/build":
				_ = w.WriteJSONBuild(201, func(dst []byte) []byte {
					dst = append(dst, `{"body":`...)
					dst = append(dst, '"')
					dst = append(dst, r.Body...)
					return append(dst, `"}`...)
				})
			case "/git":
				_ = w.WriteGitResponse(200, "application/x-git-receive-pack-result", []byte("ok"))
			default:
				_ = w.WriteJSON(404, []byte(`{"message":"missing"}`))
			}
		},
	}

	var sink memSink
	var c connState
	c.init(&sink)

	stream := "POST /build HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"3;ext=1\r\nabc\r\n2\r\nde\r\n0\r\nX-Trailer: ignore\r\n\r\n" +
		"POST /git HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\nping" +
		"GET /other HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"

	// Feed in tiny 3-byte fragments to stress header/body/chunk FSM resumption.
	for i := 0; i < len(stream); i += 3 {
		end := i + 3
		if end > len(stream) {
			end = len(stream)
		}
		chunk := stream[i:end]
		if c.filled+len(chunk) > len(c.readBuf) {
			c.ensureReadCap(c.filled + len(chunk))
		}
		copy(c.readBuf[c.filled:], chunk)
		c.filled += len(chunk)
		keep := srv.processReadBuffer(&c)
		if end < len(stream) && !keep {
			t.Fatalf("unexpected close at offset %d", end)
		}
		if end == len(stream) && keep {
			t.Fatalf("expected Connection: close at end of stream")
		}
	}

	got := sink.out.String()
	if !strings.Contains(got, "HTTP/1.1 100 Continue\r\n\r\n") {
		t.Fatalf("missing 100 Continue in %q", got)
	}
	if !strings.Contains(got, `{"body":"abcde"}`) {
		t.Fatalf("missing chunked build response in %q", got)
	}
	if !strings.Contains(got, "Content-Type: application/x-git-receive-pack-result\r\nContent-Length: 2\r\n\r\nok") {
		t.Fatalf("missing minimal git response in %q", got)
	}
	if !strings.Contains(got, "Connection: close\r\n") || !strings.Contains(got, `{"message":"missing"}`) {
		t.Fatalf("missing final 404 close response in %q", got)
	}
	if srv.ActivitySeq.Load() != 3 {
		t.Fatalf("expected ActivitySeq=3, got %d", srv.ActivitySeq.Load())
	}
}

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
}

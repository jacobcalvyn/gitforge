package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitforge/internal/git"
	"gitforge/internal/graphql"
	"gitforge/internal/http1"
	"gitforge/internal/mcp"
	"gitforge/internal/rest"
	"gitforge/internal/store"
)

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(800)
	debug.SetMemoryLimit(700 << 20)

	listenFlag := flag.String("listen", "", "Listen address (local standalone mode, e.g. 127.0.0.1:8080)")
	tokenFlag := flag.String("token", "", "Bearer token override (defaults to MAKEFUN_SHOP_TOKEN)")
	flag.Parse()

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	}
	listenAddr := *listenFlag
	if listenAddr == "" {
		listenAddr = os.Getenv("FORGE_LISTEN")
	}
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	if listenAddr == "" && envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	st := store.New()
	authCheck := buildAuthChecker(token)

	srv := &http1.Server{
		Handler: func(w *http1.ResponseWriter, r *http1.Request) {
			p := r.Path
			if p == "/healthz" && r.Method == "GET" {
				_ = w.WriteJSON(200, []byte(`{"ok":true}`+"\n"))
				return
			}

			if !authCheck(r.Auth) {
				_ = w.WriteJSON(401, []byte(`{"message":"Requires authentication"}`+"\n"))
				return
			}

			if strings.HasPrefix(p, "/api/v3") {
				rest.HandleREST(w, r, st)
				return
			}
			if p == "/api/graphql" {
				graphql.HandleGraphQL(w, r, st)
				return
			}
			if p == "/mcp" {
				mcp.HandleMCP(w, r, st)
				return
			}

			if dotGit := strings.Index(p, ".git/"); dotGit > 1 && p[0] == '/' {
				ownerRepo := p[1:dotGit]
				if slash := strings.IndexByte(ownerRepo, '/'); slash > 0 && !strings.ContainsRune(ownerRepo[slash+1:], '/') {
					owner := ownerRepo[:slash]
					repoName := ownerRepo[slash+1:]
					suffix := p[dotGit+4:]
					git.HandleGit(w, r, st, owner, repoName, suffix)
					return
				}
			}

			_ = w.WriteJSON(404, []byte(`{"message":"Not Found"}`+"\n"))
		},
	}

	bindTarget := listenAddr
	if bindTarget == "" {
		bindTarget = ":0"
	}
	ln, err := net.Listen("tcp", bindTarget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] failed to listen on %s: %v\n", bindTarget, err)
		os.Exit(1)
	}

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] listener has no TCP address")
		os.Exit(1)
	}
	port := tcpAddr.Port
	fmt.Printf("[entry] listening on port %d\n", port)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	if listenAddr != "" && envURL == "" {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sigCh:
			srv.Close(ln)
			os.Exit(0)
		case err := <-serveErr:
			if err != nil {
				fmt.Fprintf(os.Stderr, "[entry] serve error: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	if err := registerWithEnv(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		srv.Close(ln)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	srv.Close(ln)
	fmt.Println("[entry] done")
}

func buildAuthChecker(token string) func(header string) bool {
	if token == "" {
		return func(header string) bool {
			return header != ""
		}
	}
	bearer := "Bearer " + token
	ghToken := "token " + token
	return func(header string) bool {
		if header == bearer || header == ghToken {
			return true
		}
		if strings.HasPrefix(header, "Basic ") {
			dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
			if err == nil {
				if idx := bytes.IndexByte(dec, ':'); idx >= 0 {
					return string(dec[idx+1:]) == token
				}
			}
		}
		return false
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	ms := 250 * (1 << attempt)
	if ms > 2000 {
		ms = 2000
	}
	return time.Duration(ms) * time.Millisecond
}

func registerWithEnv(envURL string, port int) error {
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		status, respBody, err := httpDo("POST", envURL+"/register", body, 30*time.Second)
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", status, string(respBody))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for attempt := 0; ; attempt++ {
		select {
		case <-sigCh:
			return
		default:
		}
		status, respBody, err := httpDo("GET", envURL+"/done", nil, 30*time.Second)
		if err == nil && status >= 200 && status < 300 {
			var parsed struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(respBody, &parsed) == nil && parsed.Done {
				return
			}
			attempt = 0
			time.Sleep(250 * time.Millisecond)
			continue
		}
		time.Sleep(retryDelay(attempt))
	}
}

// httpDo is a minimal pure-TCP HTTP/1.1 client for MAKEFUN_ENV_URL (/register and /done),
// avoiding net/http + crypto/tls to keep the compiled static binary compact.
func httpDo(method, rawURL string, body []byte, timeout time.Duration) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	var reqBuf bytes.Buffer
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", method, path, u.Host)
	if body != nil {
		fmt.Fprintf(&reqBuf, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	reqBuf.WriteString("\r\n")
	if body != nil {
		reqBuf.Write(body)
	}
	if _, err := conn.Write(reqBuf.Bytes()); err != nil {
		return 0, nil, err
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return 0, nil, errors.New("malformed HTTP status line")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, err
	}

	cl := -1
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if colon := strings.IndexByte(trimmed, ':'); colon > 0 {
			k := strings.TrimSpace(trimmed[:colon])
			v := strings.TrimSpace(trimmed[colon+1:])
			if strings.EqualFold(k, "Content-Length") {
				cl, _ = strconv.Atoi(v)
			} else if strings.EqualFold(k, "Transfer-Encoding") && strings.Contains(strings.ToLower(v), "chunked") {
				chunked = true
			}
		}
	}

	if chunked {
		var out bytes.Buffer
		for {
			szLine, err := br.ReadString('\n')
			if err != nil {
				return status, out.Bytes(), err
			}
			szText := strings.TrimSpace(szLine)
			if semi := strings.IndexByte(szText, ';'); semi >= 0 {
				szText = strings.TrimSpace(szText[:semi])
			}
			n, err := strconv.ParseInt(szText, 16, 64)
			if err != nil {
				return status, out.Bytes(), err
			}
			if n == 0 {
				break
			}
			chunk := make([]byte, n)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return status, out.Bytes(), err
			}
			out.Write(chunk)
			_, _ = br.ReadString('\n')
		}
		return status, out.Bytes(), nil
	}

	if cl >= 0 {
		out := make([]byte, cl)
		if cl > 0 {
			if _, err := io.ReadFull(br, out); err != nil {
				return status, nil, err
			}
		}
		return status, out, nil
	}

	out, err := io.ReadAll(br)
	return status, out, err
}

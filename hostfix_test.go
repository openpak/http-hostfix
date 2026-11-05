package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a strict net/http server (it refuses a repeated Host header, like Traefik).
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		fmt.Fprintf(w, "%s %s %d %s", r.Method, r.Host, len(b), hex.EncodeToString(sum[:8]))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		f := w.(http.Flusher)
		chunk := bytes.Repeat([]byte("y"), 64<<10)
		for i := 0; i < 64; i++ { // 4 MiB, chunked
			w.Write(chunk)
			f.Flush()
		}
	})
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// shim starts a Proxy in front of addr and returns its address and log.
func shim(t *testing.T, addr string, pp bool) (string, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	p := &Proxy{Upstream: addr, ProxyProtocol: pp, Log: log.New(lb, "", 0)}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(l)
	t.Cleanup(func() { l.Close() })
	return l.Addr().String(), lb
}

func roundTrip(t *testing.T, addr string, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	return string(b)
}

func hostOf(u string) string { return strings.TrimPrefix(u, "http://") }

const wiiLogin = "POST /echo?secret=x HTTP/1.1\r\nHost: nas.openpak.org\r\nUser-Agent: Nintendo\r\nHost: nas.openpak.org\r\nHTTP_X_GAMECD: RMCP\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 11\r\nConnection: close\r\n\r\naction=test"

func TestRepeatedHostIsDropped(t *testing.T) {
	up := upstream(t)
	// Without the shim the strict server refuses the Wii's request.
	if got := roundTrip(t, hostOf(up.URL), wiiLogin); !strings.HasPrefix(got, "HTTP/1.1 400") {
		t.Fatalf("direct: %q", got[:min(40, len(got))])
	}
	addr, lb := shim(t, hostOf(up.URL), false)
	got := roundTrip(t, addr, wiiLogin)
	if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, "POST nas.openpak.org 11 ") {
		t.Fatalf("through the shim: %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	l := lb.String()
	if !strings.Contains(l, "http POST nas.openpak.org /echo -> 200") || !strings.Contains(l, "repeated Host dropped") {
		t.Fatalf("log: %q", l)
	}
	if strings.Contains(l, "secret") || strings.Contains(l, "action=test") {
		t.Fatalf("log leaks the query or the body: %q", l)
	}
}

func TestKeepAliveBodiesAndHead(t *testing.T) {
	up := upstream(t)
	addr, _ := shim(t, hostOf(up.URL), false)
	raw := "POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello" +
		"HEAD /echo HTTP/1.1\r\nHost: b\r\n\r\n" +
		"GET /nocontent HTTP/1.1\r\nHost: c\r\n\r\n" +
		"POST /echo HTTP/1.1\r\nHost: d\r\nHost: d\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2;x=y\r\nde\r\n0\r\nTrailer: v\r\n\r\n" +
		"GET /echo HTTP/1.1\r\nHost: e\r\nConnection: close\r\n\r\n"
	got := roundTrip(t, addr, raw)
	if n := strings.Count(got, "HTTP/1.1 "); n != 5 {
		t.Fatalf("%d responses, want 5: %q", n, got)
	}
	for _, want := range []string{"POST a 5 ", "HTTP/1.1 204", "POST d 5 ", "GET e 0 "} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestLargeBodiesAreIntact(t *testing.T) {
	up := upstream(t)
	addr, _ := shim(t, hostOf(up.URL), false)
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	sum := sha256.Sum256(body)
	got := roundTrip(t, addr, fmt.Sprintf("PUT /echo HTTP/1.1\r\nHost: x\r\nHost: x\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body))
	if !strings.Contains(got, fmt.Sprintf("PUT x %d %s", len(body), hex.EncodeToString(sum[:8]))) {
		t.Fatalf("upload: %q", got[:min(200, len(got))])
	}
	resp, err := http.Get("http://" + addr + "/big")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != 4<<20 {
		t.Fatalf("download %d bytes", n)
	}
}

func TestExpectContinue(t *testing.T) {
	up := upstream(t)
	addr, _ := shim(t, hostOf(up.URL), false)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write([]byte("POST /echo HTTP/1.1\r\nHost: x\r\nHost: x\r\nContent-Length: 4\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n"))
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("want 100 Continue, got %q", line)
	}
	c.Write([]byte("abcd"))
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "HTTP/1.1 200") || !strings.Contains(string(rest), "POST x 4 ") {
		t.Fatalf("final: %q", rest)
	}
}

// rawUpstream accepts one connection and hands it to fn.
func rawUpstream(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fn(c)
	}()
	return l.Addr().String()
}

func TestUpgradeStreamsRaw(t *testing.T) {
	up := rawUpstream(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		for { // read the request head
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		buf := make([]byte, 5)
		io.ReadFull(br, buf)
		c.Write(append([]byte("echo:"), buf...))
	})
	addr, _ := shim(t, up, false)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nhello"))
	got, _ := io.ReadAll(c)
	if !strings.Contains(string(got), "101 Switching") || !strings.HasSuffix(string(got), "echo:hello") {
		t.Fatalf("upgrade: %q", got)
	}
}

func TestNonHTTPPassesThrough(t *testing.T) {
	up := rawUpstream(t, func(c net.Conn) {
		b := make([]byte, 7)
		io.ReadFull(c, b)
		c.Write(append([]byte("got "), b...))
	})
	addr, _ := shim(t, up, false)
	if got := roundTrip(t, addr, "\x16\x03\x01junk"); got != "got \x16\x03\x01junk" {
		t.Fatalf("raw: %q", got)
	}
}

func TestProxyV2Header(t *testing.T) {
	hdr := make(chan []byte, 1)
	up := rawUpstream(t, func(c net.Conn) {
		b := make([]byte, 28)
		io.ReadFull(c, b)
		hdr <- b
	})
	addr, _ := shim(t, up, true)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	var b []byte
	select {
	case b = <-hdr:
	case <-time.After(5 * time.Second):
		t.Fatal("no PROXY header")
	}
	if !bytes.Equal(b[:12], []byte("\r\n\r\n\x00\r\nQUIT\n")) || b[12] != 0x21 || b[13] != 0x11 || binary.BigEndian.Uint16(b[14:16]) != 12 {
		t.Fatalf("header %x", b)
	}
	src := c.LocalAddr().(*net.TCPAddr)
	if !net.IP(b[16:20]).Equal(src.IP) || int(binary.BigEndian.Uint16(b[24:26])) != src.Port {
		t.Fatalf("source %v:%d, want %v", net.IP(b[16:20]), binary.BigEndian.Uint16(b[24:26]), src)
	}
}

func TestRawStreamsWithoutNewlines(t *testing.T) {
	up := rawUpstream(t, func(c net.Conn) {
		b := make([]byte, 3)
		io.ReadFull(c, b)
		c.Write([]byte("\x00\x01ack")) // no newline, connection stays open
		io.ReadFull(c, b)
	})
	addr, _ := shim(t, up, false)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("\x05\x01\x00"))
	got := make([]byte, 5)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "\x00\x01ack" {
		t.Fatalf("raw reply before close: %q %v", got, err)
	}
}

func TestManyParallelKeepAliveClients(t *testing.T) {
	up := upstream(t)
	addr, _ := shim(t, hostOf(up.URL), false)
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}, Timeout: 10 * time.Second}
			for j := 0; j < 20; j++ {
				body := strings.Repeat("z", i*j)
				resp, err := cl.Post("http://"+addr+"/echo", "text/plain", strings.NewReader(body))
				if err != nil {
					errs <- err
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if !strings.Contains(string(b), fmt.Sprintf(" %d ", len(body))) {
					errs <- fmt.Errorf("client %d request %d: %q", i, j, b)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

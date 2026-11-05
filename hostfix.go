// Package main is http-hostfix: a pass-through in front of the box's plain-HTTP (port 80)
// entrypoint. Every Wii DWC title sends its NAS login with the Host header twice; Traefik's
// HTTP parser answers 400 to that, so no Wii could sign in (prds#144). The shim drops repeated
// Host lines from each request head and otherwise streams bytes through unchanged, both ways.
// It opens its upstream connection with a PROXY v2 header carrying the visitor's address, the
// way openpak-edge does, so Traefik still sees who is calling.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxHead     = 64 << 10        // bytes of request or response head before we stop parsing
	headTimeout = 2 * time.Minute // idle time allowed before the next request head
)

// reqInfo is what the response side needs about the request it answers.
type reqInfo struct {
	method, host, path string
	dupHost            bool
	raw                bool // the request side stopped parsing: stream the rest both ways
	start              time.Time
}

// Proxy is one configured shim.
type Proxy struct {
	Upstream      string
	ProxyProtocol bool
	Log           *log.Logger
	Dial          func(network, addr string) (net.Conn, error) // nil: net.DialTimeout
}

func (p *Proxy) dial() (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial("tcp", p.Upstream)
	}
	return net.DialTimeout("tcp", p.Upstream, 5*time.Second)
}

// Serve accepts connections until l is closed.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go p.handle(c)
	}
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	u, err := p.dial()
	if err != nil {
		p.Log.Printf("upstream dial: %v", err)
		return
	}
	defer u.Close()
	if p.ProxyProtocol {
		if _, err := u.Write(proxyV2(c.RemoteAddr(), c.LocalAddr())); err != nil {
			return
		}
	}
	q := make(chan reqInfo, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.responses(bufio.NewReaderSize(u, 32<<10), c, q)
		closeWrite(c)
	}()
	p.requests(bufio.NewReaderSize(c, 32<<10), c, u, q)
	close(q)
	closeWrite(u)
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// readHead reads a message head (start line and headers, through the blank line) as raw lines.
// ok is false when the head does not look like HTTP/1.x or is too big; raw then holds every
// byte read so far, so the caller can fall back to a plain byte stream.
func readHead(br *bufio.Reader) (lines [][]byte, raw []byte, ok bool, err error) {
	for {
		line, err := br.ReadSlice('\n')
		raw = append(raw, line...)
		if err == bufio.ErrBufferFull || len(raw) > maxHead {
			return nil, raw, false, nil
		}
		if err != nil {
			if len(raw) == 0 {
				return nil, nil, false, err
			}
			return nil, raw, false, err
		}
		l := append([]byte(nil), line...)
		if len(lines) == 0 && len(bytes.TrimSpace(l)) == 0 {
			continue // stray CRLF between messages
		}
		lines = append(lines, l)
		if len(bytes.TrimRight(l, "\r\n")) == 0 {
			return lines, raw, true, nil
		}
	}
}

func headerName(line []byte) string {
	if i := bytes.IndexByte(line, ':'); i > 0 {
		return strings.ToLower(strings.TrimSpace(string(line[:i])))
	}
	return ""
}

func headerValue(line []byte) string {
	if i := bytes.IndexByte(line, ':'); i > 0 {
		return strings.TrimSpace(string(bytes.TrimRight(line[i+1:], "\r\n")))
	}
	return ""
}

// framing is how a message body ends.
type framing int

const (
	bodyNone framing = iota
	bodyLength
	bodyChunked
	bodyUntilClose
	bodyRaw // the connection stops being HTTP (Upgrade, CONNECT)
)

// requests reads requests from the client, drops repeated Host lines and writes them upstream.
func (p *Proxy) requests(br *bufio.Reader, c, u net.Conn, q chan<- reqInfo) {
	for {
		_ = c.SetReadDeadline(time.Now().Add(headTimeout))
		if b, err := br.Peek(1); err == nil && !(b[0] >= 'A' && b[0] <= 'Z') && b[0] != '\r' && b[0] != '\n' {
			// Not an HTTP request line (methods are upper-case tokens): stream it as is.
			_ = c.SetReadDeadline(time.Time{})
			q <- reqInfo{method: "?", raw: true, start: time.Now()}
			_, _ = io.Copy(u, br)
			return
		}
		lines, raw, ok, err := readHead(br)
		_ = c.SetReadDeadline(time.Time{})
		if !ok {
			if len(raw) > 0 {
				// Not something we understand: pass it and everything after it through.
				q <- reqInfo{method: "?", raw: true, start: time.Now()}
				_, _ = u.Write(raw)
				if err == nil {
					_, _ = io.Copy(u, br)
				}
			}
			return
		}
		info := reqInfo{start: time.Now()}
		if f := strings.Fields(string(lines[0])); len(f) >= 2 {
			info.method, info.path = f[0], f[1]
		}
		var out bytes.Buffer
		out.Write(lines[0])
		hosts, length, chunked, upgrade := 0, int64(-1), false, false
		for _, l := range lines[1:] {
			switch headerName(l) {
			case "host":
				hosts++
				if hosts > 1 {
					info.dupHost = true
					continue // the fix: one Host line only
				}
				info.host = headerValue(l)
			case "content-length":
				if n, err := strconv.ParseInt(headerValue(l), 10, 64); err == nil {
					length = n
				}
			case "transfer-encoding":
				chunked = strings.Contains(strings.ToLower(headerValue(l)), "chunked")
			case "upgrade":
				upgrade = true
			}
			out.Write(l)
		}
		if _, err := u.Write(out.Bytes()); err != nil {
			return
		}
		q <- info
		switch {
		case upgrade || strings.EqualFold(info.method, "CONNECT"):
			_, _ = io.Copy(u, br)
			return
		case chunked:
			if copyChunked(u, br) != nil {
				return
			}
		case length > 0:
			if _, err := io.CopyN(u, br, length); err != nil {
				return
			}
		}
	}
}

// responses reads responses from upstream, writes them to the client and logs each one.
func (p *Proxy) responses(br *bufio.Reader, c net.Conn, q <-chan reqInfo) {
	var cur *reqInfo
	for {
		// A response answers a request: wait for it, so a request that went raw is streamed
		// raw from the first byte back.
		if cur == nil {
			// Wait for the next request, but notice the upstream closing meanwhile (a
			// "Connection: close" exchange is over: end the client's side too).
			peeked := make(chan error, 1)
			go func() { _, err := br.Peek(1); peeked <- err }()
			select {
			case r, open := <-q:
				if !open {
					if <-peeked == nil {
						_, _ = io.Copy(c, br) // anything the upstream still sends after the client left
					}
					return
				}
				cur = &r
				if r.raw {
					<-peeked
				} else if err := <-peeked; err != nil {
					return
				}
			case err := <-peeked:
				if err != nil {
					return
				}
				r, open := <-q // bytes arrived before the request was queued
				if !open {
					_, _ = io.Copy(c, br)
					return
				}
				cur = &r
			}
		}
		if cur.raw {
			_, _ = io.Copy(c, br)
			p.logRequest(cur, 0)
			return
		}
		lines, raw, ok, _ := readHead(br)
		if !ok {
			if len(raw) > 0 {
				_, _ = c.Write(raw)
				_, _ = io.Copy(c, br)
			}
			return
		}
		code := 0
		if f := strings.Fields(string(lines[0])); len(f) >= 2 {
			code, _ = strconv.Atoi(f[1])
		}
		var head bytes.Buffer
		length, chunked := int64(-1), false
		for _, l := range lines {
			switch headerName(l) {
			case "content-length":
				if n, err := strconv.ParseInt(headerValue(l), 10, 64); err == nil {
					length = n
				}
			case "transfer-encoding":
				chunked = strings.Contains(strings.ToLower(headerValue(l)), "chunked")
			}
			head.Write(l)
		}
		if _, err := c.Write(head.Bytes()); err != nil {
			return
		}
		if code >= 100 && code < 200 && code != 101 {
			continue // interim (100 Continue): the final response follows for the same request
		}
		method := ""
		if cur != nil {
			method = cur.method
		}
		var f framing
		switch {
		case code == 101 || (strings.EqualFold(method, "CONNECT") && code >= 200 && code < 300):
			f = bodyRaw
		case strings.EqualFold(method, "HEAD") || code == 204 || code == 304:
			f = bodyNone
		case chunked:
			f = bodyChunked
		case length >= 0:
			f = bodyLength
		default:
			f = bodyUntilClose
		}
		var err error
		switch f {
		case bodyLength:
			_, err = io.CopyN(c, br, length)
		case bodyChunked:
			err = copyChunked(c, br)
		case bodyUntilClose, bodyRaw:
			_, err = io.Copy(c, br)
		}
		p.logRequest(cur, code)
		cur = nil
		if err != nil || f == bodyUntilClose || f == bodyRaw {
			return
		}
	}
}

// logRequest writes one line per request: method, host, path (no query), status, time.
// Never bodies, query strings or header values other than the host.
func (p *Proxy) logRequest(r *reqInfo, code int) {
	if r == nil {
		p.Log.Printf("http ? ? ? -> %d", code)
		return
	}
	path := r.path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if len(path) > 200 {
		path = path[:200]
	}
	host := r.host
	if len(host) > 120 {
		host = host[:120]
	}
	fix := ""
	if r.dupHost {
		fix = " (repeated Host dropped)"
	}
	p.Log.Printf("http %s %s %s -> %d %dms%s", r.method, host, path, code, time.Since(r.start).Milliseconds(), fix)
}

// copyChunked copies one chunked body, chunk-size lines, data and trailers, byte for byte.
func copyChunked(w io.Writer, br *bufio.Reader) error {
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return errors.New("chunk size line")
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		sz := strings.TrimSpace(string(line))
		if i := strings.IndexByte(sz, ';'); i >= 0 {
			sz = sz[:i]
		}
		n, err := strconv.ParseInt(sz, 16, 64)
		if err != nil || n < 0 {
			return errors.New("bad chunk size")
		}
		if n == 0 {
			for { // trailers, then the blank line
				t, err := br.ReadSlice('\n')
				if err != nil {
					return err
				}
				if _, err := w.Write(t); err != nil {
					return err
				}
				if len(bytes.TrimRight(t, "\r\n")) == 0 {
					return nil
				}
			}
		}
		if _, err := io.CopyN(w, br, n+2); err != nil { // data and its CRLF
			return err
		}
	}
}

// proxyV2 is a PROXY protocol v2 header for a TCP connection from src to dst.
func proxyV2(src, dst net.Addr) []byte {
	sig := []byte("\r\n\r\n\x00\r\nQUIT\n")
	s, sok := src.(*net.TCPAddr)
	d, dok := dst.(*net.TCPAddr)
	if !sok || !dok {
		return append(sig, 0x20, 0x00, 0, 0) // LOCAL: no address
	}
	out := append([]byte(nil), sig...)
	if s4, d4 := s.IP.To4(), d.IP.To4(); s4 != nil && d4 != nil {
		out = append(out, 0x21, 0x11)
		out = binary.BigEndian.AppendUint16(out, 12)
		out = append(out, s4...)
		out = append(out, d4...)
	} else {
		out = append(out, 0x21, 0x21)
		out = binary.BigEndian.AppendUint16(out, 36)
		out = append(out, s.IP.To16()...)
		out = append(out, d.IP.To16()...)
	}
	out = binary.BigEndian.AppendUint16(out, uint16(s.Port))
	out = binary.BigEndian.AppendUint16(out, uint16(d.Port))
	return out
}

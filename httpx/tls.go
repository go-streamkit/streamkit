// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// TLSFingerprint names the client whose TLS ClientHello the transport imitates.
//
// A TLS handshake announces who is calling long before the first header does.
// crypto/tls sends a ClientHello no browser sends — its own cipher order, its
// own extension set, no GREASE — and some servers close the connection on
// sight of it, before any request is written and whatever the headers would
// have said. Setting Config.TLSFingerprint makes the transport present the
// hello of the named browser instead, using a TLS stack that can reproduce one
// byte for byte. The zero value keeps Go's own hello: nothing moves on the
// wire for a caller who does not ask.
//
// # HTTP/2
//
// A browser hello offers "h2" and "http/1.1" over ALPN, so a client that sends
// one and then speaks HTTP/1.1 whatever the server picked is telling a second,
// louder lie. net/http cannot do the switch here: it hands a connection to its
// HTTP/2 machinery only after a type assertion to *tls.Conn, which the
// fingerprinted connection is not, so it would quietly stay on HTTP/1.1. The
// transport therefore does the ALPN dispatch itself. It remembers the protocol
// each authority negotiated and hands the request either to net/http's
// Transport or to golang.org/x/net/http2's, both of them dialling through the
// same fingerprinted handshake. The connection opened to learn the protocol is
// not thrown away: it is parked on the request context and picked up by
// whichever of the two dials next, so nothing is handshaked twice.
//
// A proxy is honoured for https by opening the tunnel in the dial, with a
// CONNECT request, because net/http reaches for the proxy's address rather
// than the target's once a custom TLS dialler is in play — and would then wrap
// the fingerprinted handshake in one of its own. http:// requests have no
// hello to shape and keep going through the ordinary transport.
type TLSFingerprint string

// The fingerprints Config.TLSFingerprint accepts. The zero value keeps Go's
// own ClientHello, which is what every caller that does not ask gets.
const (
	// FingerprintDefault leaves crypto/tls to send its own hello.
	FingerprintDefault TLSFingerprint = ""
	FingerprintChrome  TLSFingerprint = "chrome"
	FingerprintFirefox TLSFingerprint = "firefox"
	FingerprintSafari  TLSFingerprint = "safari"
	FingerprintEdge    TLSFingerprint = "edge"
)

// helloIDs maps a fingerprint name to the hello the TLS stack should send. The
// _Auto members follow the library's idea of the current release of each
// browser, which is what an old hello would give away.
var helloIDs = map[TLSFingerprint]utls.ClientHelloID{
	FingerprintChrome:  utls.HelloChrome_Auto,
	FingerprintFirefox: utls.HelloFirefox_Auto,
	FingerprintSafari:  utls.HelloSafari_Auto,
	FingerprintEdge:    utls.HelloEdge_Auto,
}

// ErrUnknownFingerprint reports a Config.TLSFingerprint that names no client.
// It is returned by New: a name that is not one is a mistake in the caller,
// and quietly falling back to the hello the caller was trying to avoid would
// leave them debugging a server that keeps refusing them.
var ErrUnknownFingerprint = errors.New("httpx: unknown TLS fingerprint")

// TLSFingerprints lists the names Config.TLSFingerprint accepts, in a stable
// order, so a command line can print them.
func TLSFingerprints() []TLSFingerprint {
	names := make([]TLSFingerprint, 0, len(helloIDs))
	for name := range helloIDs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// helloIDFor resolves a fingerprint name. The name is matched case-insensitively
// because it usually arrives from a flag or an environment variable.
func helloIDFor(name TLSFingerprint) (utls.ClientHelloID, error) {
	id, ok := helloIDs[TLSFingerprint(strings.ToLower(string(name)))]
	if !ok {
		known := make([]string, 0, len(helloIDs))
		for _, n := range TLSFingerprints() {
			known = append(known, string(n))
		}
		return utls.ClientHelloID{}, fmt.Errorf("%w %q; known names are %s",
			ErrUnknownFingerprint, string(name), strings.Join(known, ", "))
	}
	return id, nil
}

// browserTransport is the http.RoundTripper Client uses when a fingerprint is
// asked for.
type browserTransport struct {
	hello      utls.ClientHelloID
	roots      *x509.CertPool
	proxy      func(*http.Request) (*url.URL, error)
	dialer     *net.Dialer
	plain      *http.Transport  // http:// URLs, where there is no hello to shape
	h1         *http.Transport  // https:// URLs that negotiated http/1.1
	h2         *http2.Transport // https:// URLs that negotiated h2
	mu         sync.Mutex
	negotiated map[string]string
}

// newBrowserTransport wraps base, the transport Client would otherwise have
// used. base keeps carrying plain-HTTP requests, proxy included; its clone
// carries the fingerprinted HTTPS ones, with the proxy moved into the dial
// because net/http reaches for the proxy address, not the target's, once a
// custom TLS dialler is in play.
// The authority pool is passed in rather than parsed here: New has already read
// it, and reading the same certificates a second time would be work done twice
// to reach an error that cannot happen.
func newBrowserTransport(cfg Config, roots *x509.CertPool, base *http.Transport) (*browserTransport, error) {
	hello, err := helloIDFor(cfg.TLSFingerprint)
	if err != nil {
		return nil, err
	}
	t := &browserTransport{
		hello:      hello,
		roots:      roots,
		proxy:      base.Proxy,
		dialer:     &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
		plain:      base,
		negotiated: map[string]string{},
	}
	h1 := base.Clone()
	h1.Proxy = nil
	h1.ForceAttemptHTTP2 = false
	h1.DialTLSContext = t.dialFor(protoHTTP1)
	t.h1 = h1
	t.h2 = &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return t.dialFor(http2.NextProtoTLS)(ctx, network, addr)
		},
	}
	return t, nil
}

// protoHTTP1 is the ALPN name of HTTP/1.1, and the protocol a server that
// names none is speaking.
const protoHTTP1 = "http/1.1"

// alpnProto reduces what a server answered to the two protocols this transport
// can speak. A server that negotiated nothing at all is speaking HTTP/1.1.
func alpnProto(negotiated string) string {
	if negotiated == http2.NextProtoTLS {
		return http2.NextProtoTLS
	}
	return protoHTTP1
}

// RoundTrip sends req over the protocol its authority negotiated.
func (t *browserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.plain.RoundTrip(req)
	}
	addr := authorityAddr(req.URL)
	proto, known := t.recall(addr)
	if !known {
		conn, negotiated, err := t.handshake(req.Context(), addr)
		if err != nil {
			return nil, err
		}
		proto = t.remember(addr, negotiated)
		park := &parkedConn{conn: conn}
		defer park.discard()
		req = req.Clone(context.WithValue(req.Context(), parkedKey{}, park))
	}
	if proto == http2.NextProtoTLS {
		return t.h2.RoundTrip(req)
	}
	return t.h1.RoundTrip(req)
}

func (t *browserTransport) recall(addr string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	proto, ok := t.negotiated[addr]
	return proto, ok
}

func (t *browserTransport) remember(addr, negotiated string) string {
	proto := alpnProto(negotiated)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.negotiated[addr] = proto
	return proto
}

// dialFor returns the TLS dialler of the sub-transport that speaks want. A
// connection that came back speaking the other protocol is refused rather than
// used: writing HTTP/1.1 into a connection the server agreed to speak HTTP/2
// on is not a request, it is a stall.
func (t *browserTransport) dialFor(want string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		if park, ok := ctx.Value(parkedKey{}).(*parkedConn); ok {
			if conn := park.take(); conn != nil {
				return conn, nil
			}
		}
		conn, negotiated, err := t.handshake(ctx, addr)
		if err != nil {
			return nil, err
		}
		if proto := t.remember(addr, negotiated); proto != want {
			conn.Close()
			return nil, fmt.Errorf("httpx: %s negotiated %s where %s was expected", addr, proto, want)
		}
		return conn, nil
	}
}

// handshake opens one fingerprinted TLS connection to addr and reports the
// protocol the server chose over ALPN.
func (t *browserTransport) handshake(ctx context.Context, addr string) (net.Conn, string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("httpx: address %q: %w", addr, err)
	}
	raw, err := t.dialRaw(ctx, addr)
	if err != nil {
		return nil, "", err
	}
	// NextProtos is deliberately left empty: the ALPN list belongs to the
	// hello being imitated, and overriding it here would put a list no
	// browser sends inside a browser's hello.
	conn := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: t.roots}, t.hello)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, "", fmt.Errorf("httpx: tls handshake with %s: %w", addr, err)
	}
	return conn, conn.ConnectionState().NegotiatedProtocol, nil
}

// dialRaw opens the TCP connection the handshake runs over, through the proxy
// when one is configured.
func (t *browserTransport) dialRaw(ctx context.Context, addr string) (net.Conn, error) {
	proxy, err := t.proxyFor(addr)
	if err != nil {
		return nil, err
	}
	if proxy == nil {
		return t.dialer.DialContext(ctx, "tcp", addr)
	}
	conn, err := t.dialer.DialContext(ctx, "tcp", proxyAddr(proxy))
	if err != nil {
		return nil, fmt.Errorf("httpx: dial proxy %s: %w", proxy.Host, err)
	}
	if err := connectTunnel(ctx, conn, addr, proxy); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// proxyFor asks the transport's proxy function what to do with addr. Only a
// plain HTTP proxy can carry the tunnel: anything else would either wrap the
// fingerprinted handshake in a second one or need a protocol this package does
// not speak, and both are worth an error rather than a silent fallback.
func (t *browserTransport) proxyFor(addr string) (*url.URL, error) {
	proxy, err := t.proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil {
		return nil, fmt.Errorf("httpx: proxy for %s: %w", addr, err)
	}
	if proxy == nil {
		return nil, nil
	}
	if proxy.Scheme != "http" {
		return nil, fmt.Errorf("httpx: a %s proxy cannot carry a fingerprinted TLS connection", proxy.Scheme)
	}
	return proxy, nil
}

func proxyAddr(proxy *url.URL) string {
	if proxy.Port() != "" {
		return proxy.Host
	}
	return net.JoinHostPort(proxy.Hostname(), "80")
}

// connectTunnel turns conn into a tunnel to addr with a CONNECT request.
func connectTunnel(ctx context.Context, conn net.Conn, addr string, proxy *url.URL) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if user := proxy.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("httpx: CONNECT %s: %w", addr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("httpx: CONNECT %s: %w", addr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("httpx: proxy refused a tunnel to %s: %s", addr, resp.Status)
	}
	if br.Buffered() > 0 {
		return fmt.Errorf("httpx: proxy sent %d bytes before the tunnel to %s opened", br.Buffered(), addr)
	}
	return nil
}

// authorityAddr is the host:port both sub-transports key their connection
// pools by, so a parked connection is found again under the same name.
func authorityAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// parkedConn carries the connection opened to learn a server's ALPN choice
// from RoundTrip to whichever sub-transport dials next. Handing it over is a
// one-shot: whoever takes it owns it, and RoundTrip closes it if nobody did.
type parkedKey struct{}

type parkedConn struct {
	mu   sync.Mutex
	conn net.Conn
}

func (p *parkedConn) take() net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	conn := p.conn
	p.conn = nil
	return conn
}

func (p *parkedConn) discard() {
	if conn := p.take(); conn != nil {
		conn.Close()
	}
}

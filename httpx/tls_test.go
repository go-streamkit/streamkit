// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// --- the ClientHello capture harness ------------------------------------
//
// A test that only asserts "no error" would prove nothing here: the whole
// claim is that the bytes on the wire changed. So the tests below stand up a
// TLS server that records the ClientHello and refuses the handshake, and
// assert on what the client actually sent.

type capturedHello struct {
	CipherSuites      []uint16
	Extensions        []uint16
	SupportedCurves   []tls.CurveID
	SupportedProtos   []string
	SupportedVersions []uint16
	SignatureSchemes  []tls.SignatureScheme
	ServerName        string
}

// helloRecorder is a TLS listener that keeps the first ClientHello it is sent.
type helloRecorder struct {
	ln   net.Listener
	seen chan capturedHello
}

func newHelloRecorder(t *testing.T) *helloRecorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &helloRecorder{ln: ln, seen: make(chan capturedHello, 8)}
	t.Cleanup(func() { ln.Close() })
	go r.serve()
	return r
}

func (r *helloRecorder) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			// Refusing from GetConfigForClient aborts the handshake as soon
			// as the hello has been read, which is all this server is for.
			srv := tls.Server(conn, &tls.Config{
				GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
					r.seen <- capturedHello{
						CipherSuites:      slices.Clone(info.CipherSuites),
						Extensions:        slices.Clone(info.Extensions),
						SupportedCurves:   slices.Clone(info.SupportedCurves),
						SupportedProtos:   slices.Clone(info.SupportedProtos),
						SupportedVersions: slices.Clone(info.SupportedVersions),
						SignatureSchemes:  slices.Clone(info.SignatureSchemes),
						ServerName:        info.ServerName,
					}
					return nil, errors.New("the hello is all this server wanted")
				},
			})
			_ = srv.HandshakeContext(context.Background())
		}()
	}
}

func (r *helloRecorder) url() string { return "https://" + r.ln.Addr().String() + "/" }

func (r *helloRecorder) next(t *testing.T) capturedHello {
	t.Helper()
	select {
	case h := <-r.seen:
		return h
	case <-time.After(30 * time.Second):
		t.Fatal("no ClientHello reached the server")
		return capturedHello{}
	}
}

// helloOf drives one handshake through a Client built from cfg and returns
// what the server saw. The handshake is meant to fail; the hello is the point.
func helloOf(t *testing.T, cfg Config) capturedHello {
	t.Helper()
	rec := newHelloRecorder(t)
	cfg.Retries = 0
	cfg.RateLimit = -1
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Get(context.Background(), rec.url(), nil); err == nil {
		t.Fatal("a server that refuses every handshake answered a request")
	}
	return rec.next(t)
}

// isGREASE reports whether v is one of the reserved values a browser sprinkles
// through its hello to keep middleboxes honest. Go's crypto/tls sends none.
func isGREASE(v uint16) bool { return v&0x0f0f == 0x0a0a && byte(v>>8) == byte(v) }

func hasGREASE(h capturedHello) bool {
	if slices.ContainsFunc(h.CipherSuites, isGREASE) || slices.ContainsFunc(h.Extensions, isGREASE) {
		return true
	}
	return slices.ContainsFunc(h.SupportedCurves, func(c tls.CurveID) bool { return isGREASE(uint16(c)) })
}

// The extension numbers the assertions below name. Keeping them here rather
// than as bare integers is what makes the assertions readable.
const (
	extPadding                 = 21
	extSignatureAlgorithmsCert = 50
	extPSKKeyExchangeModes     = 45
	extCompressCertificate     = 27
	extRecordSizeLimit         = 28
	extDelegatedCredentials    = 34
	extEncryptedClientHello    = 65037
	extApplicationSettingsNew  = 17613
)

// --- the default must not move ------------------------------------------

// TestDefaultHelloIsStillGoOwn is the guard on every caller who never sets the
// option: the hello a default Client sends must be, field for field, the one
// crypto/tls sends on its own through the transport this package has always
// built. Comparing against a live crypto/tls handshake rather than against a
// frozen list keeps the assertion true across Go releases, which a frozen list
// would not: it would go red on the next cipher the standard library reorders,
// and someone would "fix" it by copying whatever the code now does.
func TestDefaultHelloIsStillGoOwn(t *testing.T) {
	mine := helloOf(t, Config{})

	rec := newHelloRecorder(t)
	// Built the way this package built its transport before there was an
	// option at all.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 32
	tr.ForceAttemptHTTP2 = true
	resp, err := (&http.Client{Transport: tr}).Get(rec.url())
	if err == nil {
		resp.Body.Close()
		t.Fatal("a server that refuses every handshake answered a request")
	}
	stock := rec.next(t)

	if !slices.Equal(mine.CipherSuites, stock.CipherSuites) {
		t.Errorf("cipher suites moved:\n got %v\nwant %v", mine.CipherSuites, stock.CipherSuites)
	}
	if !slices.Equal(mine.Extensions, stock.Extensions) {
		t.Errorf("extensions moved:\n got %v\nwant %v", mine.Extensions, stock.Extensions)
	}
	if !slices.Equal(mine.SupportedCurves, stock.SupportedCurves) {
		t.Errorf("curves moved:\n got %v\nwant %v", mine.SupportedCurves, stock.SupportedCurves)
	}
	if !slices.Equal(mine.SignatureSchemes, stock.SignatureSchemes) {
		t.Errorf("signature schemes moved:\n got %v\nwant %v", mine.SignatureSchemes, stock.SignatureSchemes)
	}
	if !slices.Equal(mine.SupportedVersions, stock.SupportedVersions) {
		t.Errorf("versions moved:\n got %v\nwant %v", mine.SupportedVersions, stock.SupportedVersions)
	}
	if !slices.Equal(mine.SupportedProtos, stock.SupportedProtos) {
		t.Errorf("ALPN moved:\n got %v\nwant %v", mine.SupportedProtos, stock.SupportedProtos)
	}
	// And the two marks that separate Go from every browser below.
	if hasGREASE(mine) {
		t.Error("the default hello grew GREASE values, which crypto/tls does not send")
	}
	if !slices.Contains(mine.Extensions, uint16(extSignatureAlgorithmsCert)) {
		t.Error("the default hello lost signature_algorithms_cert, which crypto/tls sends and no browser does")
	}
}

// --- and a browser hello must really be one ------------------------------

// TestBrowserHelloIsBrowserShaped asserts the bytes, not the absence of an
// error: a different cipher-suite ordering from Go's, the extensions Go never
// sends, and for the fingerprints that use them, the GREASE values Go never
// sends either.
func TestBrowserHelloIsBrowserShaped(t *testing.T) {
	stock := helloOf(t, Config{})

	cases := []struct {
		name TLSFingerprint
		// grease is whether this client sprinkles GREASE. Firefox does not,
		// and asserting it did would be asserting a fiction.
		grease bool
		// mustHave are extensions this client sends and crypto/tls does not.
		mustHave []uint16
	}{
		{FingerprintChrome, true, []uint16{extCompressCertificate, extEncryptedClientHello, extApplicationSettingsNew}},
		{FingerprintFirefox, false, []uint16{extRecordSizeLimit, extDelegatedCredentials, extEncryptedClientHello}},
		{FingerprintSafari, true, []uint16{extCompressCertificate, extPadding}},
		{FingerprintEdge, true, []uint16{extCompressCertificate, extPadding}},
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			got := helloOf(t, Config{TLSFingerprint: tc.name})

			if slices.Equal(got.CipherSuites, stock.CipherSuites) {
				t.Fatalf("the cipher suites are Go's own, in Go's own order: %v", got.CipherSuites)
			}
			if slices.Equal(got.Extensions, stock.Extensions) {
				t.Fatalf("the extension list is Go's own: %v", got.Extensions)
			}
			// Every one of these clients offers the TLS 1.3 suites first;
			// Go lists them last. Ordering, not membership, is what a
			// fingerprint is read from.
			if i := slices.Index(got.CipherSuites, tls.TLS_AES_128_GCM_SHA256); i > 1 {
				t.Errorf("TLS_AES_128_GCM_SHA256 sits at %d; a browser puts it first", i)
			}
			if j := slices.Index(stock.CipherSuites, tls.TLS_AES_128_GCM_SHA256); j <= 1 {
				t.Errorf("crypto/tls now leads with TLS_AES_128_GCM_SHA256 too (index %d); the ordering assertion above is no longer telling the two apart", j)
			}
			if got := hasGREASE(got); got != tc.grease {
				t.Errorf("GREASE = %v, want %v", got, tc.grease)
			}
			for _, ext := range tc.mustHave {
				if !slices.Contains(got.Extensions, ext) {
					t.Errorf("extension %d missing from %v", ext, got.Extensions)
				}
				if slices.Contains(stock.Extensions, ext) {
					t.Errorf("extension %d is sent by crypto/tls too, so it proves nothing", ext)
				}
			}
			// psk_key_exchange_modes is sent by all four and by no Go
			// client; signature_algorithms_cert is the other way round.
			if !slices.Contains(got.Extensions, uint16(extPSKKeyExchangeModes)) {
				t.Errorf("psk_key_exchange_modes missing from %v", got.Extensions)
			}
			if slices.Contains(got.Extensions, uint16(extSignatureAlgorithmsCert)) {
				t.Errorf("signature_algorithms_cert is in %v, and no browser sends it", got.Extensions)
			}
			// The offer still has to be usable: h2 first, then HTTP/1.1.
			if !slices.Equal(got.SupportedProtos, []string{http2.NextProtoTLS, protoHTTP1}) {
				t.Errorf("ALPN = %v, want the browser's own h2/http1.1 offer", got.SupportedProtos)
			}
			// A browser sends no SNI for an IP literal, and neither does
			// this: an SNI carrying an address would be a tell of its own.
			if got.ServerName != "" {
				t.Errorf("ServerName = %q, want none for an IP literal", got.ServerName)
			}
		})
	}
}

// TestFingerprintsDifferFromEachOther keeps the table above honest: four names
// that all produced the same bytes would pass every assertion in it.
func TestFingerprintsDifferFromEachOther(t *testing.T) {
	seen := map[string]TLSFingerprint{}
	for _, name := range TLSFingerprints() {
		h := helloOf(t, Config{TLSFingerprint: name})
		key := fmt.Sprint(h.CipherSuites, h.Extensions, h.SupportedCurves, h.SupportedVersions)
		if other, ok := seen[key]; ok {
			t.Errorf("%s and %s send the same hello", name, other)
		}
		seen[key] = name
	}
}

// --- naming -------------------------------------------------------------

func TestUnknownFingerprintIsRefusedWhenTheClientIsBuilt(t *testing.T) {
	c, err := New(Config{TLSFingerprint: "netscape"})
	if !errors.Is(err, ErrUnknownFingerprint) {
		t.Fatalf("err = %v, want ErrUnknownFingerprint", err)
	}
	if c != nil {
		t.Error("a client was handed back with a fingerprint it cannot honour")
	}
	// The message has to say what the caller may write instead.
	for _, name := range TLSFingerprints() {
		if !strings.Contains(err.Error(), string(name)) {
			t.Errorf("the error does not mention %q: %v", name, err)
		}
	}
}

func TestFingerprintNamesAreCaseInsensitive(t *testing.T) {
	if _, err := New(Config{TLSFingerprint: "ChRoMe"}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestTLSFingerprintsIsTheWholeSortedSet(t *testing.T) {
	got := TLSFingerprints()
	want := []TLSFingerprint{FingerprintChrome, FingerprintEdge, FingerprintFirefox, FingerprintSafari}
	if !slices.Equal(got, want) {
		t.Fatalf("TLSFingerprints() = %v, want %v", got, want)
	}
	if slices.Contains(got, FingerprintDefault) {
		t.Error("the empty name is not a fingerprint one can ask for")
	}
}

// --- end to end ---------------------------------------------------------

func rootsOf(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// TestFingerprintedRequestSpeaksBothProtocols is the other half of the claim:
// the hello offers h2 and http/1.1, so whichever the server picks has to be
// the one actually spoken. A transport that sent a browser hello and then
// always spoke HTTP/1.1 would be a different lie.
func TestFingerprintedRequestSpeaksBothProtocols(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
		major int
	}{{"http/1.1", false, 1}, {"h2", true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "served over %s", r.Proto)
			}))
			srv.EnableHTTP2 = tc.http2
			srv.StartTLS()
			defer srv.Close()

			c := newTestClient(t, Config{
				TLSFingerprint: FingerprintChrome,
				RootCAs:        rootsOf(t, srv),
				RateLimit:      -1,
			})
			body, resp, err := c.Get(context.Background(), srv.URL, nil)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if resp.ProtoMajor != tc.major {
				t.Fatalf("the answer came back over %s, want HTTP/%d", resp.Proto, tc.major)
			}
			if want := fmt.Sprintf("served over HTTP/%d", tc.major); !strings.HasPrefix(string(body), want) {
				t.Fatalf("body = %q, want it to start with %q", body, want)
			}
			// A second request must take the same road without a second
			// handshake being needed to work out which one it is.
			if _, resp2, err := c.Get(context.Background(), srv.URL, nil); err != nil {
				t.Fatalf("second Get: %v", err)
			} else if resp2.ProtoMajor != tc.major {
				t.Fatalf("the second answer came back over %s", resp2.Proto)
			}
		})
	}
}

// TestFingerprintedRequestStillRetriesAndWaits checks the layer above the
// handshake is untouched: the same retry policy and the same per-host budget
// apply through the fingerprinted transport.
func TestFingerprintedRequestStillRetriesAndWaits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, "at last")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// Two requests per second, one at a time: three attempts cannot finish
	// in under a second unless the limiter was skipped.
	c := newTestClient(t, Config{
		TLSFingerprint: FingerprintFirefox,
		RootCAs:        rootsOf(t, srv),
		Retries:        4,
		Backoff:        time.Millisecond,
		RateLimit:      2,
		Burst:          1,
	})
	start := time.Now()
	body, resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "at last" || resp.ProtoMajor != 2 {
		t.Fatalf("body = %q over %s", body, resp.Proto)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("the server saw %d requests, want the 502s retried", got)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("three attempts at 2/s took %s: the limiter was bypassed", elapsed)
	}
}

// TestFingerprintedTransportLeavesPlainHTTPAlone: there is no hello to shape
// on an http:// URL, so those requests keep going through the transport the
// client would have used anyway, proxy settings and all.
func TestFingerprintedTransportLeavesPlainHTTPAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain")
	}))
	defer srv.Close()
	c := newTestClient(t, Config{TLSFingerprint: FingerprintSafari, RateLimit: -1})
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "plain" {
		t.Fatalf("body = %q", body)
	}
}

func TestFingerprintedRequestReportsAHandshakeThatFails(t *testing.T) {
	// A TLS server whose certificate this client has no reason to trust.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	c := newTestClient(t, Config{TLSFingerprint: FingerprintChrome, RateLimit: -1})
	_, _, err := c.Get(context.Background(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "tls handshake") {
		t.Fatalf("err = %v, want the handshake failure reported", err)
	}
}

func TestFingerprintedRequestReportsADialThatFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listens any more
	c := newTestClient(t, Config{TLSFingerprint: FingerprintChrome, RateLimit: -1, Retries: 1,
		Backoff: time.Millisecond})
	if _, _, err := c.Get(context.Background(), addr, nil); err == nil {
		t.Fatal("a connection to a closed port succeeded")
	}
}

// --- the pieces, reached directly ---------------------------------------

func newTestTransport(t *testing.T, cfg Config) *browserTransport {
	t.Helper()
	base := http.DefaultTransport.(*http.Transport).Clone()
	bt, err := newBrowserTransport(cfg, base)
	if err != nil {
		t.Fatalf("newBrowserTransport: %v", err)
	}
	return bt
}

func TestNewBrowserTransportRefusesAnUnknownName(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if _, err := newBrowserTransport(Config{TLSFingerprint: "lynx"}, base); !errors.Is(err, ErrUnknownFingerprint) {
		t.Fatalf("err = %v", err)
	}
}

// TestDialRefusesAConnectionThatSpeaksTheOtherProtocol covers the case the
// remembered ALPN choice cannot cover: a server that answers h2 to a dial made
// on behalf of the HTTP/1.1 side. Writing HTTP/1.1 into it would hang rather
// than fail.
func TestDialRefusesAConnectionThatSpeaksTheOtherProtocol(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome, RootCAs: rootsOf(t, srv)})
	conn, err := bt.dialFor(protoHTTP1)(context.Background(), "tcp", authorityAddr(u))
	if err == nil {
		conn.Close()
		t.Fatal("an h2 connection was handed to the HTTP/1.1 side")
	}
	if !strings.Contains(err.Error(), "negotiated h2 where http/1.1 was expected") {
		t.Fatalf("err = %v", err)
	}
	// And the same dial on behalf of the h2 side is the one that is fine.
	conn, err = bt.dialFor(http2.NextProtoTLS)(context.Background(), "tcp", authorityAddr(u))
	if err != nil {
		t.Fatalf("h2 dial: %v", err)
	}
	conn.Close()
}

// TestDialReportsAHandshakeItCannotMake covers the dial that finds nothing
// parked and cannot open a connection of its own: that is what a sub-transport
// sees when it needs a second connection to a host that has gone away.
func TestDialReportsAHandshakeItCannotMake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there any more
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome})
	if _, err := bt.dialFor(protoHTTP1)(context.Background(), "tcp", addr); err == nil {
		t.Fatal("a dial to a closed port succeeded")
	}
}

func TestDialTakesTheParkedConnection(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome})
	park := &parkedConn{conn: left}
	ctx := context.WithValue(context.Background(), parkedKey{}, park)
	// The address is unreachable on purpose: taking the parked connection is
	// the only way this dial can succeed.
	got, err := bt.dialFor(protoHTTP1)(ctx, "tcp", "192.0.2.1:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got != left {
		t.Fatal("the parked connection was not the one handed back")
	}
	// Handing it over is a one-shot.
	if park.take() != nil {
		t.Error("the parked connection was handed over twice")
	}
	park.discard() // now a no-op, and must stay one
}

func TestDiscardClosesAParkedConnectionNobodyTook(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	(&parkedConn{conn: left}).discard()
	if _, err := left.Write([]byte("x")); err == nil {
		t.Fatal("the unused connection was left open")
	}
}

func TestHandshakeRejectsAnAddressWithoutAPort(t *testing.T) {
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome})
	if _, _, err := bt.handshake(context.Background(), "example.invalid"); err == nil ||
		!strings.Contains(err.Error(), `address "example.invalid"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthorityAddrFillsInThePort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://host.invalid/a", "host.invalid:443"},
		{"https://host.invalid:8443/a", "host.invalid:8443"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := authorityAddr(u); got != tc.want {
			t.Errorf("authorityAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestALPNProtoTreatsSilenceAsHTTP11(t *testing.T) {
	if got := alpnProto(""); got != protoHTTP1 {
		t.Errorf("a server that negotiated nothing gave %q", got)
	}
	if got := alpnProto(http2.NextProtoTLS); got != http2.NextProtoTLS {
		t.Errorf("h2 became %q", got)
	}
	if got := alpnProto("spdy/3"); got != protoHTTP1 {
		t.Errorf("an unknown protocol gave %q, want it treated as HTTP/1.1", got)
	}
}

// --- through a proxy ----------------------------------------------------

// connectProxy is a CONNECT proxy that tunnels to whatever it is asked for. It
// records the request line and any credentials, so the tests can assert the
// tunnel was really opened rather than sidestepped.
type connectProxy struct {
	ln      net.Listener
	target  chan string
	auth    chan string
	refuse  int    // when non-zero, answer this status instead of tunnelling
	preface string // when set, write this before the 200, which is illegal
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln, target: make(chan string, 8), auth: make(chan string, 8)}
	t.Cleanup(func() { ln.Close() })
	go p.serve()
	return p
}

func (p *connectProxy) url() string { return "http://" + p.ln.Addr().String() }

func (p *connectProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *connectProxy) handle(conn net.Conn) {
	req, err := http.ReadRequest(newReaderOf(conn))
	if err != nil {
		conn.Close()
		return
	}
	p.target <- req.Host
	p.auth <- req.Header.Get("Proxy-Authorization")
	if p.refuse != 0 {
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", p.refuse, http.StatusText(p.refuse))
		conn.Close()
		return
	}
	upstream, err := net.Dial("tcp", req.Host)
	if err != nil {
		conn.Close()
		return
	}
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"+p.preface)
	go func() { io.Copy(upstream, conn); upstream.Close() }()
	go func() { io.Copy(conn, upstream); conn.Close() }()
}

func TestFingerprintedRequestGoesThroughACONNECTProxy(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through the tunnel")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	proxy := newConnectProxy(t)
	c := newTestClient(t, Config{
		TLSFingerprint: FingerprintChrome,
		RootCAs:        rootsOf(t, srv),
		RateLimit:      -1,
		Proxy:          strings.Replace(proxy.url(), "http://", "http://user:secret@", 1),
		// A deadline on the request is what puts one on the tunnel too.
		Timeout: 30 * time.Second,
	})
	body, resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "through the tunnel" || resp.ProtoMajor != 2 {
		t.Fatalf("body = %q over %s", body, resp.Proto)
	}
	if got := <-proxy.target; got != strings.TrimPrefix(srv.URL, "https://") {
		t.Errorf("the proxy was asked to reach %q", got)
	}
	if got := <-proxy.auth; got != "Basic dXNlcjpzZWNyZXQ=" {
		t.Errorf("Proxy-Authorization = %q", got)
	}
}

func TestTunnelReportsAProxyThatRefuses(t *testing.T) {
	proxy := newConnectProxy(t)
	proxy.refuse = http.StatusForbidden
	c := newTestClient(t, Config{TLSFingerprint: FingerprintChrome, RateLimit: -1, Proxy: proxy.url()})
	_, _, err := c.Get(context.Background(), "https://host.invalid/x", nil)
	if err == nil || !strings.Contains(err.Error(), "refused a tunnel") {
		t.Fatalf("err = %v", err)
	}
}

func TestTunnelReportsAProxyThatTalksTooEarly(t *testing.T) {
	proxy := newConnectProxy(t)
	proxy.preface = "surprise"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	c := newTestClient(t, Config{TLSFingerprint: FingerprintChrome, RateLimit: -1, Proxy: proxy.url(),
		RootCAs: rootsOf(t, srv)})
	_, _, err := c.Get(context.Background(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "before the tunnel") {
		t.Fatalf("err = %v", err)
	}
}

func TestTunnelReportsAProxyThatCannotBeReached(t *testing.T) {
	proxy := newConnectProxy(t)
	dead := proxy.url()
	proxy.ln.Close()
	c := newTestClient(t, Config{TLSFingerprint: FingerprintChrome, RateLimit: -1, Proxy: dead})
	_, _, err := c.Get(context.Background(), "https://host.invalid/x", nil)
	if err == nil || !strings.Contains(err.Error(), "dial proxy") {
		t.Fatalf("err = %v", err)
	}
}

func TestTunnelReportsAConnectionThatDiesWhileItIsBeingOpened(t *testing.T) {
	// A connection whose peer is gone fails on the write, and one that is
	// only closed after the request fails on the read: both are what a proxy
	// dropping the tunnel looks like.
	t.Run("on the write", func(t *testing.T) {
		left, right := net.Pipe()
		right.Close()
		left.Close()
		err := connectTunnel(context.Background(), left, "host.invalid:443", &url.URL{Scheme: "http", Host: "p:8080"})
		if err == nil || !strings.Contains(err.Error(), "CONNECT host.invalid:443") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("on the read", func(t *testing.T) {
		left, right := net.Pipe()
		go func() { io.Copy(io.Discard, right); right.Close() }()
		go func() { time.Sleep(50 * time.Millisecond); right.Close() }()
		err := connectTunnel(context.Background(), left, "host.invalid:443", &url.URL{Scheme: "http", Host: "p:8080"})
		if err == nil || !strings.Contains(err.Error(), "CONNECT host.invalid:443") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestProxyForRefusesAProxyItCannotTunnelThrough(t *testing.T) {
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome})
	bt.proxy = func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "socks5", Host: "127.0.0.1:1080"}, nil
	}
	_, err := bt.proxyFor("host.invalid:443")
	if err == nil || !strings.Contains(err.Error(), "a socks5 proxy cannot carry") {
		t.Fatalf("err = %v", err)
	}
}

func TestProxyForPassesOnTheLookupFailure(t *testing.T) {
	bt := newTestTransport(t, Config{TLSFingerprint: FingerprintChrome})
	boom := errors.New("no idea")
	bt.proxy = func(*http.Request) (*url.URL, error) { return nil, boom }
	if _, err := bt.proxyFor("host.invalid:443"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := bt.dialRaw(context.Background(), "host.invalid:443"); !errors.Is(err, boom) {
		t.Fatalf("dialRaw err = %v", err)
	}
}

func TestProxyAddrFillsInThePort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://proxy.invalid", "proxy.invalid:80"},
		{"http://proxy.invalid:3128", "proxy.invalid:3128"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyAddr(u); got != tc.want {
			t.Errorf("proxyAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRootCAsAreTrustedByTheDefaultTransportToo: the option exists for both
// roads out, not only the fingerprinted one.
func TestRootCAsAreTrustedByTheDefaultTransportToo(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "trusted")
	}))
	defer srv.Close()
	c := newTestClient(t, Config{RootCAs: rootsOf(t, srv), RateLimit: -1})
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "trusted" {
		t.Fatalf("body = %q", body)
	}
}

func newReaderOf(conn net.Conn) *bufio.Reader { return bufio.NewReader(conn) }

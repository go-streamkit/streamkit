// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewDefaults(t *testing.T) {
	c := newTestClient(t, Config{Retries: -3})
	got := c.Config()
	if got.UserAgent != DefaultUserAgent || got.Timeout != 60*time.Second ||
		got.Retries != 0 || got.Backoff != 300*time.Millisecond || got.MaxBodyBytes != 32<<20 {
		t.Fatalf("defaults = %+v", got)
	}
	c2 := newTestClient(t, Config{UserAgent: "x", Timeout: time.Second, Retries: 2,
		Backoff: time.Millisecond, MaxBodyBytes: 10})
	if got := c2.Config(); got.UserAgent != "x" || got.MaxBodyBytes != 10 {
		t.Fatalf("explicit config = %+v", got)
	}
}

func TestNewProxy(t *testing.T) {
	if _, err := New(Config{Proxy: "http://127.0.0.1:3128"}); err != nil {
		t.Fatalf("valid proxy: %v", err)
	}
	if _, err := New(Config{Proxy: "http://%zz"}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
}

func TestNewRequestHeaders(t *testing.T) {
	c := newTestClient(t, Config{UserAgent: "godl-test", Cookies: []string{"a=1", "b=2"}})
	req, err := c.NewRequest(context.Background(), "https://example.com/videos/x-42", map[string]string{
		"Referer": "https://override/",
		"Accept":  "text/html",
	})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if got := req.Header.Get("User-Agent"); got != "godl-test" {
		t.Errorf("UA = %q", got)
	}
	if got := req.Header.Get("Cookie"); got != "a=1; b=2" {
		t.Errorf("Cookie = %q", got)
	}
	if got := req.Header.Get("Referer"); got != "https://override/" {
		t.Errorf("caller header must win, got %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/html" {
		t.Errorf("Accept = %q", got)
	}
	if got := req.Header.Get("Accept-Language"); got == "" {
		t.Error("Accept-Language not set")
	}
}

func TestNewRequestDefaultRefererIsTheOrigin(t *testing.T) {
	c := newTestClient(t, Config{})
	req, err := c.NewRequest(context.Background(), "https://example.com/a/b?c=d", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if got := req.Header.Get("Referer"); got != "https://example.com/" {
		t.Errorf("Referer = %q", got)
	}
}

func TestNewRequestBadURL(t *testing.T) {
	c := newTestClient(t, Config{})
	if _, err := c.NewRequest(context.Background(), "http://%zz", nil); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestOriginOfUnusableURL(t *testing.T) {
	if got := origin("not a url"); got != "" {
		t.Errorf("origin = %q, want empty", got)
	}
	if got := origin("/relative/only"); got != "" {
		t.Errorf("origin = %q, want empty", got)
	}
}

func TestDoRetriesServerErrorsThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.Write([]byte("payload"))
		}
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Retries: 4, Backoff: time.Millisecond})
	body, resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "payload" || resp.StatusCode != 200 {
		t.Fatalf("body = %q, status = %d", body, resp.StatusCode)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

func TestDoGivesUpAfterRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Retries: 2, Backoff: time.Millisecond})
	_, _, err := c.Get(context.Background(), srv.URL, nil)
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("err = %v, want ErrHTTPStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502 StatusError", err)
	}
	if !strings.Contains(se.Error(), "502") {
		t.Errorf("Error() = %q", se.Error())
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3 (1 try + 2 retries)", got)
	}
}

func TestDoDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Retries: 5, Backoff: time.Millisecond})
	_, _, err := c.Get(context.Background(), srv.URL, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want a 404 StatusError", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server saw %d requests, want 1: a 404 is final", got)
	}
}

func TestDoTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens any more
	c := newTestClient(t, Config{Retries: 1, Backoff: time.Millisecond})
	if _, _, err := c.Get(context.Background(), url, nil); err == nil {
		t.Fatal("connection to a closed port succeeded")
	}
}

func TestDoStopsOnCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Retries: 10, Backoff: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := c.Get(ctx, srv.URL, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}

func TestDoCancelledDuringRequest(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()
	c := newTestClient(t, Config{Retries: 3, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, _, err := c.Get(ctx, srv.URL, nil); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestGetCapsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()
	c := newTestClient(t, Config{MaxBodyBytes: 10})
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(body) != 10 {
		t.Fatalf("read %d bytes, want the 10-byte cap", len(body))
	}
}

func TestGetBadURL(t *testing.T) {
	c := newTestClient(t, Config{})
	if _, _, err := c.Get(context.Background(), "http://%zz", nil); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestGetReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "50")
		w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// closing early makes the client read fail mid-body
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	defer srv.Close()
	c := newTestClient(t, Config{})
	if _, _, err := c.Get(context.Background(), srv.URL, nil); err == nil {
		t.Fatal("truncated body accepted")
	}
}

func TestStatusErrorIs(t *testing.T) {
	e := &StatusError{Code: 403, URL: "https://x"}
	if !errors.Is(e, ErrHTTPStatus) {
		t.Error("StatusError must match ErrHTTPStatus")
	}
	if errors.Is(e, errors.New("other")) {
		t.Error("StatusError must not match an unrelated error")
	}
}

// TestGetRetriesABodyCutInTheMiddle covers the commonest way a long download
// goes wrong: the answer starts arriving and the connection is reset. That is
// not a failure of the request — the same request asked again usually works —
// and forty-seven videos out of forty-nine is what not retrying it costs.
func TestGetRetriesABodyCutInTheMiddle(t *testing.T) {
	const want = "the whole body, arriving in one piece"
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			// A length is promised and the connection goes away halfway
			// through it, which is what a reset looks like to a reader.
			w.Header().Set("Content-Length", strconv.Itoa(len(want)))
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, want[:10])
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		io.WriteString(w, want)
	}))
	defer srv.Close()

	c, err := New(Config{Retries: 3, Backoff: time.Millisecond, RateLimit: -1})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("the server was asked %d times, want 3", got)
	}
}

// TestGetGivesUpOnABodyThatNeverArrives checks the retries are bounded and the
// failure is reported, rather than a short body being handed back as whole.
func TestGetGivesUpOnABodyThatNeverArrives(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "short")
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	c, err := New(Config{Retries: 2, Backoff: time.Millisecond, RateLimit: -1})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatalf("a body of %d bytes was handed back as whole", len(body))
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("the server was asked %d times, want the try and its two retries", got)
	}
}

// TestGetStopsWhenTheCallerGivesUp covers the two ways a caller ends a retried
// read: while it waits between attempts, and while the body is arriving. Both
// must report the giving-up rather than keep asking the site for a file nobody
// is waiting for any more.
func TestGetStopsWhenTheCallerGivesUp(t *testing.T) {
	t.Run("between attempts", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			// The head is sent whole and flushed, so the answer reaches the
			// caller intact and it is the body that stops: that is the
			// failure this retry exists for, and it happens where the body
			// is read, not where the request is made.
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "short")
			w.(http.Flusher).Flush()
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()
		// The wait is staged rather than timed: a deadline that has to fall
		// inside it takes another path under load, and a test that covers a
		// different thing on a busy machine covers nothing.
		original := sleep
		defer func() { sleep = original }()
		gone := errors.New("httpx: the caller gave up")
		sleep = func(context.Context, time.Duration) error { return gone }

		c, err := New(Config{Retries: 5, Backoff: time.Millisecond, RateLimit: -1})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Get(context.Background(), srv.URL, nil); !errors.Is(err, gone) {
			t.Fatalf("err = %v, want the giving-up reported", err)
		}
		// It gave up in the wait, so the site was asked once and not five
		// times over.
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Errorf("the site was asked %d times after the caller left", got)
		}
	})

	t.Run("while the body is arriving", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "some")
			w.(http.Flusher).Flush()
			<-release
		}))
		defer func() { close(release); srv.Close() }()
		c, err := New(Config{Retries: 3, Backoff: time.Millisecond, RateLimit: -1})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, _, err := c.Get(ctx, srv.URL, nil); err == nil {
			t.Fatal("a body cut short by the caller was accepted")
		}
	})
}

// TestConfigSurvivesTheWire pins a property this struct has to keep: callers
// hand it across process boundaries to describe one HTTP policy to another
// program, and gob is what carries it there.
//
// It is not a hypothetical. A field of a type gob cannot encode — one with no
// exported field of its own — compiles, passes every test about what it does,
// and breaks every caller that sends the config anywhere. This test is what
// notices, and it fills the struct rather than sampling it, so a field added
// later is covered by having been set here.
func TestConfigSurvivesTheWire(t *testing.T) {
	// A real certificate, because the config that comes back has to still
	// build a client, and a placeholder would only prove the encoding.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	want := Config{
		UserAgent: "godl/1",
		Cookies:   []string{"a=1", "b=2"},
		Proxy:     "http://proxy.example:3128",
		Timeout:   30 * time.Second,
		// Deliberately not the same as Timeout: the two waits are separate
		// values, and a round trip that swapped them would come back
		// looking right.
		StallTimeout:   45 * time.Second,
		Retries:        3,
		Backoff:        time.Second,
		MaxBodyBytes:   1 << 20,
		RateLimit:      2.5,
		Burst:          4,
		BulkRateLimit:  10,
		BulkBurst:      2,
		MaxRetryAfter:  time.Minute,
		TLSFingerprint: FingerprintChrome,
		RootCAsPEM:     rootsOf(t, srv),
	}
	// Every field is set above, so a field added later and left out here shows
	// up as a zero on the other side rather than passing unnoticed.
	if unset := zeroFields(want); len(unset) > 0 {
		t.Fatalf("this test does not set %s, so it does not cover them", strings.Join(unset, ", "))
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(want); err != nil {
		t.Fatalf("a config cannot be sent: %v", err)
	}
	var got Config
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("a config cannot be read back: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("came back as %+v, want %+v", got, want)
	}
	// And what came back still builds a client, which is the point of sending
	// it in the first place.
	if _, err := New(got); err != nil {
		t.Fatalf("the config that crossed cannot be used: %v", err)
	}
}

// zeroFields names the fields of a config that are still at their zero value,
// so a test claiming to fill one can be held to it.
func zeroFields(c Config) []string {
	v := reflect.ValueOf(c)
	var out []string
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			out = append(out, v.Type().Field(i).Name)
		}
	}
	return out
}

// TestRootCAsThatAreNotCertificatesAreRefused covers the other half of taking
// authorities as text: text that holds no certificate must be refused, not
// quietly replaced by the host's own trust.
func TestRootCAsThatAreNotCertificatesAreRefused(t *testing.T) {
	for _, fp := range []TLSFingerprint{FingerprintDefault, FingerprintChrome} {
		_, err := New(Config{RootCAsPEM: []byte("nothing certificate-shaped"), TLSFingerprint: fp})
		if !errors.Is(err, ErrRootCAs) {
			t.Fatalf("fingerprint %q: err = %v, want ErrRootCAs", fp, err)
		}
	}
}

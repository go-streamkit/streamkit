// Copyright (c) 2026, the streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// window is short enough to keep these tests quick and long enough that a busy
// machine does not mistake scheduling for a stall.
const window = 200 * time.Millisecond

// TestASlowTransferIsNotAStalledOne covers the distinction the whole guard
// exists for. This transfer takes five times the timeout and must finish: what
// matters is that bytes keep arriving, not how long they take in total.
//
// It is the case a single deadline gets wrong, and getting it wrong is not
// abstract — a sixty-second cap abandoned thirty-six large downloads that were
// each arriving perfectly well.
func TestASlowTransferIsNotAStalledOne(t *testing.T) {
	const parts = 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		for range parts {
			w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(window / 2)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Timeout: window, Retries: 0, RateLimit: -1})
	req, err := c.NewRequest(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading a slow but healthy transfer: %v", err)
	}
	if len(got) != parts {
		t.Fatalf("read %d bytes, want %d", len(got), parts)
	}
	if took := time.Since(started); took < window {
		t.Fatalf("the transfer took %s, less than the timeout: it never tested anything", took)
	}
}

// TestATransferThatStopsIsCutOff covers the other half: a connection that
// answered and then went quiet is not waited on for ever.
func TestATransferThatStopsIsCutOff(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-release
	}))
	// Registered first, so it runs last: deferred calls unwind in reverse.
	// A server waits for its handlers before it closes, and this handler
	// waits to be released, so releasing it has to happen first — getting
	// this the wrong way round hangs the test binary instead of failing it.
	defer srv.Close()
	defer close(release)

	c := newTestClient(t, Config{Timeout: window, Retries: 0, RateLimit: -1})
	req, err := c.NewRequest(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	started := time.Now()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a transfer that stopped arriving was read to the end")
	}
	// Noticed by sampling, so somewhere between one and two windows, plus
	// room for a busy machine.
	if took := time.Since(started); took > 6*window {
		t.Fatalf("the stall took %s to notice, want about two windows", took)
	}
}

// TestClosingTheBodyEndsTheWatch covers the branch a finished download takes:
// the watcher must stop when the body is closed rather than wake for ever.
func TestClosingTheBodyEndsTheWatch(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelled bool
	body := guardStall(io.NopCloser(io.LimitReader(zeroes{}, 4)), time.Hour,
		func() { cancelled = true; cancel() })
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !cancelled {
		t.Error("closing the body left the attempt's context alive")
	}
}

// zeroes is a reader that never runs out, so a test can ask for exactly as
// much as it wants.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestASilentProxyIsNotWaitedOnForEver covers a deadline that was lost by
// accident and found by a coverage number.
//
// The CONNECT exchange never had a deadline of its own: it inherited one from
// the deadline over the whole request. Taking that away — which is the point
// of this change — left a proxy free to accept the connection, say nothing,
// and hold the dial open for as long as it liked. Nothing failed to prove it;
// a branch simply stopped being reached.
func TestASilentProxyIsNotWaitedOnForEver(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accepted, and then nothing: no answer to CONNECT, ever.
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()

	c := newTestClient(t, Config{
		TLSFingerprint: FingerprintChrome, RateLimit: -1, Retries: 0,
		Timeout: window, Proxy: "http://" + ln.Addr().String(),
	})
	req, err := c.NewRequest(context.Background(), "https://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := c.Do(req); err == nil {
		t.Fatal("a proxy that never answered CONNECT was waited on to the end")
	}
	if took := time.Since(started); took > 10*window {
		t.Fatalf("the silent proxy held the dial for %s, want about one window", took)
	}
}

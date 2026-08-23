// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestBulkMarker(t *testing.T) {
	ctx := context.Background()
	if IsBulk(ctx) {
		t.Error("a plain context must not be bulk")
	}
	if !IsBulk(Bulk(ctx)) {
		t.Error("Bulk did not mark the context")
	}
}

func TestBurstAndLimitDefaults(t *testing.T) {
	if got := limitOf(0); got != rate.Inf {
		t.Errorf("limitOf(0) = %v, want no limit", got)
	}
	if got := limitOf(-1); got != rate.Inf {
		t.Errorf("limitOf(-1) = %v, want no limit", got)
	}
	if got := float64(limitOf(2.5)); got != 2.5 {
		t.Errorf("limitOf(2.5) = %v", got)
	}
	cases := []struct {
		burst int
		rate  float64
		want  int
	}{{0, 4, 4}, {0, 0.5, 1}, {0, 1, 1}, {3, 100, 3}}
	for _, c := range cases {
		if got := burstOf(c.burst, c.rate); got != c.want {
			t.Errorf("burstOf(%d, %v) = %d, want %d", c.burst, c.rate, got, c.want)
		}
	}
}

func TestRateLimitSpacesPageRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	// Two requests per second, one at a time: the third must wait about a
	// second after the burst is spent.
	c := newTestClient(t, Config{RateLimit: 2, Burst: 1})
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("three requests at 2/s took %s: the limiter did not hold them", elapsed)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d requests", got)
	}
}

func TestRateLimitIsPerHost(t *testing.T) {
	// Both servers must answer, whichever name they are reached under.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer slow.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	c := newTestClient(t, Config{RateLimit: 1, Burst: 1})
	ctx := context.Background()
	// The bucket is keyed by host name, which is what a site rate-limits:
	// reach the second server under a different name for it to count apart.
	otherName := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	start := time.Now()
	if _, _, err := c.Get(ctx, slow.URL, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, otherName, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("two hosts shared one bucket: %s", elapsed)
	}
}

func TestBulkTrafficIsNotHeldByThePageLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// A page budget of one request per second, and no bulk limit at all.
	c := newTestClient(t, Config{RateLimit: 1, Burst: 1})
	ctx := Bulk(context.Background())
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, _, err := c.Get(ctx, srv.URL, nil); err != nil {
			t.Fatalf("bulk request %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bulk traffic was throttled like page traffic: %s", elapsed)
	}
}

func TestBulkRateLimitAppliesWhenAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := newTestClient(t, Config{RateLimit: -1, BulkRateLimit: 2, BulkBurst: 1})
	ctx := Bulk(context.Background())
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, err := c.Get(ctx, srv.URL, nil); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("the bulk limit was ignored: %s", elapsed)
	}
}

func TestRateLimitHonoursCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := newTestClient(t, Config{RateLimit: 0.5, Burst: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Get(ctx, srv.URL, nil); err != nil {
		t.Fatalf("the first request should pass: %v", err)
	}
	_, _, err := c.Get(ctx, srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("err = %v, want the limiter to report the cancellation", err)
	}
}

func TestNoLimitWhenDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := newTestClient(t, Config{RateLimit: -1})
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a disabled limiter still held requests: %s", elapsed)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}
	if d, ok := retryAfter(mk("7"), now); !ok || d != 7*time.Second {
		t.Errorf("seconds: %v, %v", d, ok)
	}
	if d, ok := retryAfter(mk(now.Add(30*time.Second).Format(http.TimeFormat)), now); !ok || d < 29*time.Second {
		t.Errorf("date: %v, %v", d, ok)
	}
	if d, ok := retryAfter(mk(now.Add(-time.Hour).Format(http.TimeFormat)), now); !ok || d != 0 {
		t.Errorf("past date: %v, %v", d, ok)
	}
	for _, v := range []string{"", "soon", "-3"} {
		if _, ok := retryAfter(mk(v), now); ok {
			t.Errorf("Retry-After %q was accepted", v)
		}
	}
	if _, ok := retryAfter(nil, now); ok {
		t.Error("a nil response yielded a delay")
	}
}

func TestDoWaitsTheRetryAfterDelay(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	// The client's own backoff is tiny: the server's request must win.
	c := newTestClient(t, Config{Retries: 2, Backoff: time.Millisecond, RateLimit: -1})
	start := time.Now()
	body, _, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("waited %s: the Retry-After header was ignored", elapsed)
	}
}

func TestDoRefusesAnAbsurdRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Retries: 3, Backoff: time.Millisecond, RateLimit: -1,
		MaxRetryAfter: time.Second})
	start := time.Now()
	_, _, err := c.Get(context.Background(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "asks to wait") {
		t.Fatalf("err = %v, want the capped Retry-After", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the client waited %s instead of giving up", elapsed)
	}
}

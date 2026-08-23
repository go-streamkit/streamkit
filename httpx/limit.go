// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Requests come in two flavours, and lumping them together would either get
// the caller banned or make downloads unusable:
//
//   - page and API requests, which a site counts and throttles. A creator with
//     fifty videos is fifty page loads plus fifty API calls, and that is what
//     trips a rate limiter.
//   - the bulk transfer of one file: hundreds of range or segment requests to
//     a CDN that expects exactly that. Capping those to a few per second would
//     divide the download speed by ten for no protection at all.
//
// So the limiter keeps one token bucket per host for each flavour, and the
// bulk one is unlimited by default.
type bulkKey struct{}

// Bulk marks a context as carrying the transfer of a file rather than a page
// request. The engines use it; a plugin never needs to.
func Bulk(ctx context.Context) context.Context {
	return context.WithValue(ctx, bulkKey{}, true)
}

// IsBulk reports whether ctx was marked by Bulk.
func IsBulk(ctx context.Context) bool {
	v, _ := ctx.Value(bulkKey{}).(bool)
	return v
}

// hostLimiter holds one token bucket per host and per flavour.
type hostLimiter struct {
	mu        sync.Mutex
	page      map[string]*rate.Limiter
	bulk      map[string]*rate.Limiter
	pageRate  rate.Limit
	pageBurst int
	bulkRate  rate.Limit
	bulkBurst int
}

func newHostLimiter(cfg Config) *hostLimiter {
	return &hostLimiter{
		page:      map[string]*rate.Limiter{},
		bulk:      map[string]*rate.Limiter{},
		pageRate:  limitOf(cfg.RateLimit),
		pageBurst: burstOf(cfg.Burst, cfg.RateLimit),
		bulkRate:  limitOf(cfg.BulkRateLimit),
		bulkBurst: burstOf(cfg.BulkBurst, cfg.BulkRateLimit),
	}
}

// limitOf turns requests per second into a rate.Limit, where zero or less
// means "no limit".
func limitOf(perSecond float64) rate.Limit {
	if perSecond <= 0 {
		return rate.Inf
	}
	return rate.Limit(perSecond)
}

// burstOf keeps a burst that is at least one request, and by default lets a
// second's worth of requests through at once.
func burstOf(burst int, perSecond float64) int {
	if burst > 0 {
		return burst
	}
	if n := int(perSecond); n > 1 {
		return n
	}
	return 1
}

// wait blocks until the host's bucket allows one more request, or until ctx is
// done. An unlimited flavour never blocks.
func (l *hostLimiter) wait(ctx context.Context, host string, bulk bool) error {
	lim := l.limiter(host, bulk)
	if lim == nil {
		return nil
	}
	if err := lim.Wait(ctx); err != nil {
		return fmt.Errorf("httpx: rate limit for %s: %w", host, err)
	}
	return nil
}

func (l *hostLimiter) limiter(host string, bulk bool) *rate.Limiter {
	limit, burst, table := l.pageRate, l.pageBurst, l.page
	if bulk {
		limit, burst, table = l.bulkRate, l.bulkBurst, l.bulk
	}
	if limit == rate.Inf {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := table[host]; ok {
		return lim
	}
	lim := rate.NewLimiter(limit, burst)
	table[host] = lim
	return lim
}

// retryAfter reads the delay a server asks for after a 429 or a 503. It
// understands both spellings of the header: seconds, and an HTTP date.
func retryAfter(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(now); d > 0 {
			return d, true
		}
		return 0, true // the date has passed: retry at once
	}
	return 0, false
}

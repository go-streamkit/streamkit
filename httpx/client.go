// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

// Package httpx is the one HTTP client shared by the host downloader and by
// every plugin: same browser-shaped headers, same retry policy, same proxy and
// cookie handling on both sides of the plugin boundary.
package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultUserAgent is a current desktop browser UA. Sites gate video sources
// on it, so it is not cosmetic.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

// Config describes a Client. The zero value is usable.
type Config struct {
	UserAgent string
	// Cookies are raw "name=value" pairs sent with every request.
	Cookies []string
	Proxy   string
	Timeout time.Duration
	Retries int
	// Backoff is the first retry delay; it doubles on each attempt.
	Backoff time.Duration
	// MaxBodyBytes caps ReadAll-style helpers. 0 means 32 MiB.
	MaxBodyBytes int64
	// RateLimit is the number of page and API requests allowed per second
	// and per host. 0 means the default, which is deliberately polite; a
	// negative value removes the limit.
	RateLimit float64
	// Burst is how many of those requests may be spent at once. 0 lets a
	// second's worth through.
	Burst int
	// BulkRateLimit does the same for the transfer of a file: its range
	// requests and its HLS segments. 0 means no limit, because the download
	// concurrency already bounds those and a CDN expects them.
	BulkRateLimit float64
	// BulkBurst is the burst of the bulk bucket.
	BulkBurst int
	// MaxRetryAfter caps how long a "Retry-After" answer can hold the
	// client. 0 means 2 minutes.
	MaxRetryAfter time.Duration
	// TLSFingerprint names the browser whose TLS ClientHello the transport
	// should present. The zero value keeps Go's own hello, which is what
	// every caller that does not ask for one keeps getting. See the
	// TLSFingerprint type for what changes on the wire, and for what it
	// means for HTTP/2.
	TLSFingerprint TLSFingerprint
	// RootCAs are the certificate authorities to trust instead of the
	// host's. nil, the usual value, trusts the host's.
	RootCAs *x509.CertPool
}

// DefaultRateLimit is the page-request budget per host: enough to walk a
// listing briskly, slow enough not to look like a hammer.
const DefaultRateLimit = 4

// Client is a retrying, rate-limited HTTP client.
type Client struct {
	cfg   Config
	http  *http.Client
	limit *hostLimiter
}

// ErrHTTPStatus reports a non-2xx response.
var ErrHTTPStatus = errors.New("httpx: bad status")

// StatusError carries the offending status code and URL.
type StatusError struct {
	Code int
	URL  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpx: bad status %d (%s) for %s", e.Code, http.StatusText(e.Code), e.URL)
}

// Is makes errors.Is(err, ErrHTTPStatus) work.
func (e *StatusError) Is(target error) bool { return target == ErrHTTPStatus }

// New builds a Client from cfg, applying the defaults.
func New(cfg Config) (*Client, error) {
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 300 * time.Millisecond
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 32 << 20
	}
	if cfg.RateLimit == 0 {
		cfg.RateLimit = DefaultRateLimit
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = 2 * time.Minute
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 32
	tr.ForceAttemptHTTP2 = true
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("httpx: proxy %q: %w", cfg.Proxy, err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	if cfg.RootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
	}
	var rt http.RoundTripper = tr
	if cfg.TLSFingerprint != FingerprintDefault {
		bt, err := newBrowserTransport(cfg, tr)
		if err != nil {
			return nil, err
		}
		rt = bt
	}
	return &Client{
		cfg:   cfg,
		http:  &http.Client{Transport: rt, Timeout: cfg.Timeout},
		limit: newHostLimiter(cfg),
	}, nil
}

// Config returns the effective configuration.
func (c *Client) Config() Config { return c.cfg }

// NewRequest builds a GET request carrying the client's browser headers.
func (c *Client) NewRequest(ctx context.Context, rawURL string, hdr map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: request %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if ref := origin(rawURL); ref != "" {
		req.Header.Set("Referer", ref)
	}
	if len(c.cfg.Cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(c.cfg.Cookies, "; "))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req, nil
}

func origin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

// Do runs a request with retries on transport errors and on 5xx/429, waiting
// for the host's rate limit before every attempt. When a server answers with
// Retry-After, that delay wins over the client's own backoff: being told to
// wait and not waiting is how an address gets blocked. The caller closes the
// returned body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	bulk := IsBulk(ctx)
	host := req.URL.Hostname()

	var lastErr error
	delay := c.cfg.Backoff
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
			delay *= 2
		}
		if err := c.limit.wait(ctx, host, bulk); err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req.Clone(ctx))
		switch {
		case err != nil:
			lastErr = fmt.Errorf("httpx: get %s: %w", req.URL, err)
			if ctx.Err() != nil {
				return nil, lastErr
			}
		case retryable(resp.StatusCode):
			if d, ok := retryAfter(resp, time.Now()); ok {
				if d > c.cfg.MaxRetryAfter {
					resp.Body.Close()
					return nil, fmt.Errorf("httpx: %s asks to wait %s, longer than the %s cap: %w",
						host, d.Round(time.Second), c.cfg.MaxRetryAfter, &StatusError{
							Code: resp.StatusCode, URL: req.URL.String()})
				}
				delay = d
			}
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, URL: req.URL.String()}
		case resp.StatusCode >= 400:
			resp.Body.Close()
			return nil, &StatusError{Code: resp.StatusCode, URL: req.URL.String()}
		default:
			return resp, nil
		}
	}
	return nil, lastErr
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// sleep waits between attempts, and gives up when the caller does. It is a
// variable so that a test can stage the giving-up: reaching it by timing means
// a test that takes another path under load, which is worse than no test.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("httpx: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// Get fetches rawURL and returns the body, capped at MaxBodyBytes.
func (c *Client) Get(ctx context.Context, rawURL string, hdr map[string]string) ([]byte, *http.Response, error) {
	req, err := c.NewRequest(ctx, rawURL, hdr)
	if err != nil {
		return nil, nil, err
	}
	// A connection reset in the middle of a body is the commonest way a long
	// download goes wrong, and it is not a failure of the request: the same
	// request asked again usually works. Do retries what fails before the
	// answer arrives; this retries what fails while it is being read.
	var lastErr error
	delay := c.cfg.Backoff
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, delay); err != nil {
				return nil, nil, err
			}
			delay *= 2
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
		resp.Body.Close()
		if err == nil {
			return body, resp, nil
		}
		lastErr = fmt.Errorf("httpx: read %s: %w", rawURL, err)
		if ctx.Err() != nil {
			return nil, resp, lastErr
		}
	}
	return nil, nil, lastErr
}

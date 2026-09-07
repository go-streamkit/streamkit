// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"net"
	"sync/atomic"
)

// traffic counts the bytes a client actually puts on the wire and takes off it.
//
// A downloader that shows what it is RECEIVING and nothing about what it SENDS
// is telling half the story. Every range request, every set of headers and
// every TLS handshake is bytes leaving the machine, and a file fetched in
// four-mebibyte chunks is hundreds of them — small next to the download, and
// not nothing, which is what a graph with an empty upload half claims.
//
// The count is taken at the CONNECTION rather than at the request, so it is the
// real thing: framing, TLS records, redirects and retries are all in it, and
// nothing has to be estimated from the size of a header map.
type traffic struct {
	sent     atomic.Int64
	received atomic.Int64
}

// countingConn reports what passes through it, in both directions.
type countingConn struct {
	net.Conn
	t *traffic
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.t.received.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.t.sent.Add(int64(n))
	}
	return n, err
}

// count wraps a dialer so every connection it opens is measured.
func (t *traffic) count(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil || c == nil {
			return c, err
		}
		return &countingConn{Conn: c, t: t}, nil
	}
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Traffic reports the bytes this client has put on the wire and taken off it
// since it was made.
//
// Sent is small beside received for a downloader, and that is the point: it is
// what a request costs, and it is measured rather than guessed.
func (c *Client) Traffic() (sent, received int64) {
	return c.traffic.sent.Load(), c.traffic.received.Load()
}

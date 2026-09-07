// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTrafficCountsBothDirections covers the half of a transfer a downloader
// never showed: what it SENDS.
//
// Every range request, every set of headers and every handshake is bytes
// leaving the machine, and a file fetched in four-mebibyte chunks is hundreds
// of them. A graph with an empty upload half claims that is nothing.
func TestTrafficCountsBothDirections(t *testing.T) {
	body := strings.Repeat("payload", 4096) // ~28 KB, well past any framing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sent, got := c.Traffic(); sent != 0 || got != 0 {
		t.Fatalf("a client that has done nothing reports %d sent, %d received", sent, got)
	}

	req, err := c.NewRequest(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	sent, got := c.Traffic()
	if sent <= 0 {
		t.Error("the request cost nothing to send, which cannot be: a request line, " +
			"headers and framing all go on the wire")
	}
	if got < n {
		t.Errorf("received %d bytes on the wire for a %d byte body, want at least the body",
			got, n)
	}
	// Sent is small beside received, and that is the shape a downloader has.
	if sent > got {
		t.Errorf("sent %d and received %d: a download that sends more than it takes "+
			"is a counter reading the wrong direction", sent, got)
	}

	// A second request adds to both, rather than starting again.
	before, beforeGot := sent, got
	req2, _ := c.NewRequest(context.Background(), srv.URL, nil)
	resp2, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	sent2, got2 := c.Traffic()
	if sent2 <= before || got2 <= beforeGot {
		t.Errorf("a second request left the counters at %d/%d, was %d/%d",
			sent2, got2, before, beforeGot)
	}
}

// Copyright (c) 2026, the streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package httpx

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// stallGuard ends a transfer that has stopped moving, and only that one.
//
// A single deadline over a whole request cannot tell a large file arriving
// slowly from a connection that died: both cross it, so whatever value is
// chosen is wrong for one of them. Sixty seconds abandons a five-hundred
// megabyte file that would have finished, and half an hour lets a dead socket
// hold a download for half an hour. What separates the two is not how long the
// transfer takes but whether anything is still arriving.
type stallGuard struct {
	body   io.ReadCloser
	moved  atomic.Bool
	cancel context.CancelFunc
	stop   chan struct{}
	once   sync.Once
}

// guardStall wraps body so that a window passing with no byte read cancels the
// request. The cancel is also called when the body is closed, so the context
// derived for this attempt does not outlive it.
func guardStall(body io.ReadCloser, window time.Duration, cancel context.CancelFunc) io.ReadCloser {
	g := &stallGuard{body: body, cancel: cancel, stop: make(chan struct{})}
	go g.watch(window)
	return g
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 {
		g.moved.Store(true)
	}
	return n, err
}

func (g *stallGuard) Close() error {
	g.once.Do(func() { close(g.stop) })
	err := g.body.Close()
	g.cancel()
	return err
}

// watch cancels once a whole window has passed with nothing read.
//
// It samples rather than measures the gap exactly, so a stall is noticed
// somewhere between one and two windows after the last byte. Knowing sooner
// would cost a timer reset on every read of every download, to hurry a case
// that is already lost.
func (g *stallGuard) watch(window time.Duration) {
	t := time.NewTicker(window)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			if !g.moved.Swap(false) {
				g.cancel()
				return
			}
		}
	}
}

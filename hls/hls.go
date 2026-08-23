// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

// Package hls parses the subset of RFC 8216 that video sites actually serve:
// master playlists with variant streams, media playlists with segments,
// EXT-X-BYTERANGE, EXT-X-MAP and AES-128 keys. It is stdlib-only so that both
// the host and a plugin can link it without pulling a demuxer in.
package hls

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ErrParse reports a malformed playlist.
var ErrParse = errors.New("hls: malformed playlist")

// Variant is one rendition announced by a master playlist.
type Variant struct {
	URL        string
	Bandwidth  int
	Width      int
	Height     int
	Codecs     string
	Name       string
	FrameRate  float64
	Resolution string
}

// Key is an EXT-X-KEY declaration. Method "NONE" means the following
// segments are clear.
type Key struct {
	Method string
	URL    string
	IV     []byte
	Format string
}

// ByteRange is an EXT-X-BYTERANGE sub-slice of a resource.
type ByteRange struct {
	Length int64
	Offset int64
}

// Header renders the Range request header value.
func (b ByteRange) Header() string {
	return fmt.Sprintf("bytes=%d-%d", b.Offset, b.Offset+b.Length-1)
}

// Segment is one media segment, or the EXT-X-MAP initialisation segment.
type Segment struct {
	URL       string
	Duration  float64
	Sequence  int
	Key       *Key
	ByteRange *ByteRange
}

// Playlist is a parsed master or media playlist.
type Playlist struct {
	Master         bool
	Version        int
	TargetDuration float64
	Variants       []Variant
	Init           *Segment // EXT-X-MAP, fragmented-MP4 streams only
	Segments       []Segment
	Live           bool // no EXT-X-ENDLIST
}

// TotalDuration sums the segment durations.
func (p *Playlist) TotalDuration() float64 {
	var t float64
	for _, s := range p.Segments {
		t += s.Duration
	}
	return t
}

// Best returns the highest-bandwidth variant of a master playlist.
func (p *Playlist) Best() (Variant, bool) {
	var best Variant
	found := false
	for _, v := range p.Variants {
		if !found || v.Bandwidth > best.Bandwidth ||
			(v.Bandwidth == best.Bandwidth && v.Height > best.Height) {
			best, found = v, true
		}
	}
	return best, found
}

// Parse decodes a playlist. base resolves relative URIs and may be nil when
// every URI in the playlist is absolute.
func Parse(base *url.URL, data []byte) (*Playlist, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, fmt.Errorf("%w: missing #EXTM3U header", ErrParse)
	}
	p := &Playlist{Live: true}
	var (
		pending  Variant
		haveVar  bool
		duration float64
		key      *Key
		br       *ByteRange
		seq      int
		nextOff  int64
	)
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			v, err := parseStreamInf(line[len("#EXT-X-STREAM-INF:"):])
			if err != nil {
				return nil, err
			}
			pending, haveVar, p.Master = v, true, true
		case strings.HasPrefix(line, "#EXTINF:"):
			d, err := parseExtinf(line[len("#EXTINF:"):])
			if err != nil {
				return nil, err
			}
			duration = d
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			k, err := parseKey(base, line[len("#EXT-X-KEY:"):])
			if err != nil {
				return nil, err
			}
			key = k
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := parseAttrs(line[len("#EXT-X-MAP:"):])
			uri, ok := attrs["URI"]
			if !ok {
				return nil, fmt.Errorf("%w: EXT-X-MAP without URI", ErrParse)
			}
			abs, err := resolve(base, uri)
			if err != nil {
				return nil, err
			}
			init := &Segment{URL: abs, Sequence: -1}
			if r, ok := attrs["BYTERANGE"]; ok {
				rng, _, err := parseByteRange(r, 0)
				if err != nil {
					return nil, err
				}
				init.ByteRange = rng
			}
			p.Init = init
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			rng, off, err := parseByteRange(line[len("#EXT-X-BYTERANGE:"):], nextOff)
			if err != nil {
				return nil, err
			}
			br, nextOff = rng, off
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			d, err := strconv.ParseFloat(strings.TrimSpace(line[len("#EXT-X-TARGETDURATION:"):]), 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad EXT-X-TARGETDURATION: %v", ErrParse, err)
			}
			p.TargetDuration = d
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			n, err := strconv.Atoi(strings.TrimSpace(line[len("#EXT-X-VERSION:"):]))
			if err != nil {
				return nil, fmt.Errorf("%w: bad EXT-X-VERSION: %v", ErrParse, err)
			}
			p.Version = n
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			n, err := strconv.Atoi(strings.TrimSpace(line[len("#EXT-X-MEDIA-SEQUENCE:"):]))
			if err != nil {
				return nil, fmt.Errorf("%w: bad EXT-X-MEDIA-SEQUENCE: %v", ErrParse, err)
			}
			seq = n
		case line == "#EXT-X-ENDLIST":
			p.Live = false
		case strings.HasPrefix(line, "#"):
			continue // comment or tag we do not need
		case haveVar:
			abs, err := resolve(base, line)
			if err != nil {
				return nil, err
			}
			pending.URL = abs
			p.Variants = append(p.Variants, pending)
			pending, haveVar = Variant{}, false
		default:
			abs, err := resolve(base, line)
			if err != nil {
				return nil, err
			}
			s := Segment{URL: abs, Duration: duration, Sequence: seq, Key: key, ByteRange: br}
			p.Segments = append(p.Segments, s)
			seq++
			duration, br = 0, nil
		}
	}
	if len(p.Variants) == 0 && len(p.Segments) == 0 {
		return nil, fmt.Errorf("%w: neither variants nor segments", ErrParse)
	}
	if p.Master {
		p.Live = false
	}
	return p, nil
}

func parseExtinf(s string) (float64, error) {
	if i := strings.Index(s, ","); i >= 0 {
		s = s[:i]
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad EXTINF duration: %v", ErrParse, err)
	}
	return d, nil
}

func parseStreamInf(s string) (Variant, error) {
	attrs := parseAttrs(s)
	v := Variant{Codecs: attrs["CODECS"], Name: attrs["NAME"], Resolution: attrs["RESOLUTION"]}
	if b, ok := attrs["BANDWIDTH"]; ok {
		n, err := strconv.Atoi(b)
		if err != nil {
			return v, fmt.Errorf("%w: bad BANDWIDTH %q", ErrParse, b)
		}
		v.Bandwidth = n
	}
	if b, ok := attrs["AVERAGE-BANDWIDTH"]; ok && v.Bandwidth == 0 {
		n, err := strconv.Atoi(b)
		if err != nil {
			return v, fmt.Errorf("%w: bad AVERAGE-BANDWIDTH %q", ErrParse, b)
		}
		v.Bandwidth = n
	}
	if r := v.Resolution; r != "" {
		w, h, ok := strings.Cut(strings.ToLower(r), "x")
		if !ok {
			return v, fmt.Errorf("%w: bad RESOLUTION %q", ErrParse, r)
		}
		wi, err1 := strconv.Atoi(w)
		hi, err2 := strconv.Atoi(h)
		if err1 != nil || err2 != nil {
			return v, fmt.Errorf("%w: bad RESOLUTION %q", ErrParse, r)
		}
		v.Width, v.Height = wi, hi
	}
	if fr, ok := attrs["FRAME-RATE"]; ok {
		f, err := strconv.ParseFloat(fr, 64)
		if err != nil {
			return v, fmt.Errorf("%w: bad FRAME-RATE %q", ErrParse, fr)
		}
		v.FrameRate = f
	}
	return v, nil
}

func parseKey(base *url.URL, s string) (*Key, error) {
	attrs := parseAttrs(s)
	method, ok := attrs["METHOD"]
	if !ok {
		return nil, fmt.Errorf("%w: EXT-X-KEY without METHOD", ErrParse)
	}
	if method == "NONE" {
		return nil, nil
	}
	k := &Key{Method: method, Format: attrs["KEYFORMAT"]}
	uri, ok := attrs["URI"]
	if !ok {
		return nil, fmt.Errorf("%w: EXT-X-KEY %s without URI", ErrParse, method)
	}
	abs, err := resolve(base, uri)
	if err != nil {
		return nil, err
	}
	k.URL = abs
	if iv, ok := attrs["IV"]; ok {
		b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0X"), "0x"))
		if err != nil {
			return nil, fmt.Errorf("%w: bad IV %q: %v", ErrParse, iv, err)
		}
		k.IV = b
	}
	return k, nil
}

func parseByteRange(s string, prevEnd int64) (*ByteRange, int64, error) {
	s = strings.TrimSpace(s)
	lenStr, offStr, hasOff := strings.Cut(s, "@")
	n, err := strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil {
		return nil, prevEnd, fmt.Errorf("%w: bad BYTERANGE %q: %v", ErrParse, s, err)
	}
	off := prevEnd
	if hasOff {
		off, err = strconv.ParseInt(strings.TrimSpace(offStr), 10, 64)
		if err != nil {
			return nil, prevEnd, fmt.Errorf("%w: bad BYTERANGE offset %q: %v", ErrParse, s, err)
		}
	}
	return &ByteRange{Length: n, Offset: off}, off + n, nil
}

// parseAttrs splits a comma-separated attribute list, honouring quoted values
// that themselves contain commas (CODECS, RESOLUTION lists).
func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	var field strings.Builder
	inQuote := false
	flush := func() {
		kv := strings.TrimSpace(field.String())
		field.Reset()
		if kv == "" {
			return
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return
		}
		out[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			field.WriteRune(r)
		case r == ',' && !inQuote:
			flush()
		default:
			field.WriteRune(r)
		}
	}
	flush()
	return out
}

func resolve(base *url.URL, ref string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("%w: bad URI %q: %v", ErrParse, ref, err)
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	if base == nil {
		return "", fmt.Errorf("%w: relative URI %q without a base", ErrParse, ref)
	}
	return base.ResolveReference(u).String(), nil
}

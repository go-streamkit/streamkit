// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package hls

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func base(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("base %q: %v", s, err)
	}
	return u
}

const master = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",FRAME-RATE=25.000
360p/index.m3u8
#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=2400000,RESOLUTION=1280x720,NAME="720p"
https://cdn.example.com/720p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080p/index.m3u8
`

func TestParseMaster(t *testing.T) {
	p, err := Parse(base(t, "https://v.example.com/hls/master.m3u8"), []byte(master))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !p.Master || len(p.Variants) != 3 || p.Version != 4 {
		t.Fatalf("got master=%v variants=%d version=%d", p.Master, len(p.Variants), p.Version)
	}
	if p.Live {
		t.Error("a master playlist must not be reported as live")
	}
	v0 := p.Variants[0]
	if v0.URL != "https://v.example.com/hls/360p/index.m3u8" {
		t.Errorf("relative variant resolved to %q", v0.URL)
	}
	if v0.Width != 640 || v0.Height != 360 || v0.Bandwidth != 800000 || v0.FrameRate != 25 {
		t.Errorf("variant 0 = %+v", v0)
	}
	if !strings.Contains(v0.Codecs, "mp4a.40.2") {
		t.Errorf("quoted CODECS list was split: %q", v0.Codecs)
	}
	if p.Variants[1].Bandwidth != 2400000 || p.Variants[1].Name != "720p" {
		t.Errorf("variant 1 = %+v", p.Variants[1])
	}
	best, ok := p.Best()
	if !ok || best.Height != 1080 {
		t.Errorf("Best() = %+v, %v", best, ok)
	}
}

const mediaPlaylist = `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0X0102030405060708090A0B0C0D0E0F10
#EXTINF:9.009,
seg0.ts
#EXTINF:8.5,title
https://cdn.example.com/seg1.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:2.0,
seg2.ts
#EXT-X-ENDLIST
`

func TestParseMediaPlaylist(t *testing.T) {
	p, err := Parse(base(t, "https://v.example.com/hls/index.m3u8"), []byte(mediaPlaylist))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Master || len(p.Segments) != 3 || p.Live {
		t.Fatalf("master=%v segments=%d live=%v", p.Master, len(p.Segments), p.Live)
	}
	if p.TargetDuration != 10 {
		t.Errorf("TargetDuration = %v", p.TargetDuration)
	}
	if got := p.TotalDuration(); got < 19.5 || got > 19.51 {
		t.Errorf("TotalDuration = %v, want ~19.509", got)
	}
	s0 := p.Segments[0]
	if s0.URL != "https://v.example.com/hls/seg0.ts" || s0.Sequence != 7 || s0.Duration != 9.009 {
		t.Errorf("segment 0 = %+v", s0)
	}
	if s0.Key == nil || s0.Key.URL != "https://v.example.com/hls/key.bin" || len(s0.Key.IV) != 16 {
		t.Errorf("segment 0 key = %+v", s0.Key)
	}
	if s0.Key.IV[0] != 1 || s0.Key.IV[15] != 0x10 {
		t.Errorf("IV = %x", s0.Key.IV)
	}
	if p.Segments[1].Sequence != 8 || p.Segments[1].URL != "https://cdn.example.com/seg1.ts" {
		t.Errorf("segment 1 = %+v", p.Segments[1])
	}
	if p.Segments[2].Key != nil {
		t.Errorf("METHOD=NONE must clear the key, got %+v", p.Segments[2].Key)
	}
	if _, ok := p.Best(); ok {
		t.Error("a media playlist has no variant")
	}
}

const fmp4 = `#EXTM3U
#EXT-X-MAP:URI="init.mp4",BYTERANGE="800@0"
#EXTINF:4.0,
#EXT-X-BYTERANGE:1000@800
media.mp4
#EXTINF:4.0,
#EXT-X-BYTERANGE:2000
media.mp4
#EXT-X-ENDLIST
`

func TestParseFragmentedMP4AndByteRanges(t *testing.T) {
	p, err := Parse(base(t, "https://v.example.com/x/index.m3u8"), []byte(fmp4))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Init == nil || p.Init.URL != "https://v.example.com/x/init.mp4" {
		t.Fatalf("Init = %+v", p.Init)
	}
	if p.Init.ByteRange == nil || p.Init.ByteRange.Header() != "bytes=0-799" {
		t.Errorf("init byte range = %+v", p.Init.ByteRange)
	}
	if got := p.Segments[0].ByteRange.Header(); got != "bytes=800-1799" {
		t.Errorf("segment 0 range = %q", got)
	}
	// A BYTERANGE without @offset continues where the previous one stopped.
	if got := p.Segments[1].ByteRange.Header(); got != "bytes=1800-3799" {
		t.Errorf("segment 1 range = %q", got)
	}
}

func TestParseLiveHasNoEndlist(t *testing.T) {
	p, err := Parse(nil, []byte("#EXTM3U\n#EXTINF:4.0,\nhttps://a/b.ts\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !p.Live {
		t.Error("a playlist without EXT-X-ENDLIST is live")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no header":           "#EXT-X-VERSION:3\n",
		"empty":               "",
		"bad extinf":          "#EXTM3U\n#EXTINF:abc,\na.ts\n",
		"bad targetduration":  "#EXTM3U\n#EXT-X-TARGETDURATION:x\n#EXTINF:1,\na.ts\n",
		"bad version":         "#EXTM3U\n#EXT-X-VERSION:x\n#EXTINF:1,\na.ts\n",
		"bad sequence":        "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:x\n#EXTINF:1,\na.ts\n",
		"bad bandwidth":       "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=x\na.m3u8\n",
		"bad avg bandwidth":   "#EXTM3U\n#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=x\na.m3u8\n",
		"bad resolution":      "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=abc\na.m3u8\n",
		"bad resolution nums": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=axb\na.m3u8\n",
		"bad frame rate":      "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,FRAME-RATE=x\na.m3u8\n",
		"key without method":  "#EXTM3U\n#EXT-X-KEY:URI=\"k\"\n#EXTINF:1,\na.ts\n",
		"key without uri":     "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128\n#EXTINF:1,\na.ts\n",
		"bad iv":              "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://k\",IV=0Xzz\n#EXTINF:1,\na.ts\n",
		"map without uri":     "#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"1@0\"\n#EXTINF:1,\na.ts\n",
		"bad map range":       "#EXTM3U\n#EXT-X-MAP:URI=\"https://i\",BYTERANGE=\"x@0\"\n#EXTINF:1,\na.ts\n",
		"bad byterange":       "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:x\na.ts\n",
		"bad range offset":    "#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:1@x\na.ts\n",
		"nothing at all":      "#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS\n",
		"relative no base":    "#EXTM3U\n#EXTINF:1,\na.ts\n",
		"bad segment uri":     "#EXTM3U\n#EXTINF:1,\nhttp://a b\x7f/c.ts\n",
		"bad variant uri":     "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttp://a\x7f b/c.m3u8\n",
		"bad key uri":         "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"ht tp://\x7f\"\n#EXTINF:1,\nhttps://a.ts\n",
		"bad map uri":         "#EXTM3U\n#EXT-X-MAP:URI=\"ht tp://\x7f\"\n#EXTINF:1,\nhttps://a.ts\n",
	}
	for name, in := range cases {
		var b *url.URL
		if name != "relative no base" {
			b = nil
		}
		_, err := Parse(b, []byte(in))
		if err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
			continue
		}
		if !errors.Is(err, ErrParse) {
			t.Errorf("%s: %v, want ErrParse", name, err)
		}
	}
}

func TestParseAttrsQuoting(t *testing.T) {
	got := parseAttrs(`METHOD=AES-128,URI="https://k?a=1,b=2",,IV=0x00,EMPTY,=x,`)
	if got["URI"] != "https://k?a=1,b=2" {
		t.Errorf("quoted URI with a comma = %q", got["URI"])
	}
	if got["METHOD"] != "AES-128" || got["IV"] != "0x00" {
		t.Errorf("attrs = %v", got)
	}
	if _, ok := got["EMPTY"]; ok {
		t.Errorf("valueless attribute kept: %v", got)
	}
}

func TestBestPrefersHeightOnEqualBandwidth(t *testing.T) {
	p := &Playlist{Variants: []Variant{
		{URL: "a", Bandwidth: 100, Height: 480},
		{URL: "b", Bandwidth: 100, Height: 720},
	}}
	best, ok := p.Best()
	if !ok || best.URL != "b" {
		t.Fatalf("Best() = %+v, %v", best, ok)
	}
}

func TestParseIgnoresUnknownTagsAndBlankLines(t *testing.T) {
	in := "#EXTM3U\n\n#EXT-X-PROGRAM-DATE-TIME:2026-08-20T00:00:00Z\n#EXTINF:1,\nhttps://a/b.ts\n#EXT-X-ENDLIST\n"
	p, err := Parse(nil, []byte(in))
	if err != nil || len(p.Segments) != 1 {
		t.Fatalf("Parse = %+v, %v", p, err)
	}
}

func TestParseCRLF(t *testing.T) {
	p, err := Parse(nil, []byte("#EXTM3U\r\n#EXTINF:1,\r\nhttps://a/b.ts\r\n#EXT-X-ENDLIST\r\n"))
	if err != nil || len(p.Segments) != 1 || p.Segments[0].URL != "https://a/b.ts" {
		t.Fatalf("Parse = %+v, %v", p, err)
	}
}

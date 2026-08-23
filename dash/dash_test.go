// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package dash

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func base(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("base %q: %v", s, err)
	}
	return u
}

// templateManifest is the common shape: separate video and audio, segments
// named by a template driven by a timeline.
const templateManifest = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT1M40S">
  <BaseURL>https://cdn.example/v1/</BaseURL>
  <Period duration="PT1M40S">
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <SegmentTemplate initialization="init-$RepresentationID$.m4s"
                       media="seg-$RepresentationID$-$Number%03d$.m4s" startNumber="1" timescale="1000">
        <SegmentTimeline>
          <S t="0" d="4000" r="2"/>
          <S d="2000"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v720" codecs="avc1.4d401f" width="1280" height="720" bandwidth="2400000"/>
      <Representation id="v1080" codecs="avc1.640028" width="1920" height="1080" bandwidth="5000000"/>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en">
      <SegmentTemplate initialization="init-a.m4s" media="seg-a-$Number$.m4s" duration="4000" timescale="1000"/>
      <Representation id="a1" codecs="mp4a.40.2" bandwidth="128000"/>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseTemplateAndTimeline(t *testing.T) {
	m, err := Parse(base(t, "https://site.example/dash/manifest.mpd"), []byte(templateManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Dynamic || m.Duration != 100*time.Second {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Streams) != 3 {
		t.Fatalf("got %d streams", len(m.Streams))
	}
	videos := m.Video()
	if len(videos) != 2 || videos[0].Height != 1080 {
		t.Fatalf("video streams = %+v", videos)
	}
	v := videos[0]
	if v.Init != "https://cdn.example/v1/init-v1080.m4s" {
		t.Errorf("Init = %q", v.Init)
	}
	// Four segments: three from the repeat, one from the trailing entry.
	if len(v.Segments) != 4 {
		t.Fatalf("segments = %v", v.Segments)
	}
	if v.Segments[0] != "https://cdn.example/v1/seg-v1080-001.m4s" {
		t.Errorf("segment 0 = %q (the %%03d padding must be honoured)", v.Segments[0])
	}
	if v.Segments[3] != "https://cdn.example/v1/seg-v1080-004.m4s" {
		t.Errorf("segment 3 = %q", v.Segments[3])
	}
	if v.Muxed {
		t.Error("a video-only representation is not muxed")
	}
	if !m.HasAudio() || !m.NeedsMuxing(v) {
		t.Error("separate audio must be reported as needing a muxer")
	}
	// The audio stream is generated from duration, not from a timeline.
	var audio Stream
	for _, s := range m.Streams {
		if s.Kind == KindAudio {
			audio = s
		}
	}
	if len(audio.Segments) != 25 { // 100s / 4s
		t.Fatalf("audio segments = %d, want 25", len(audio.Segments))
	}
	if audio.Lang != "en" {
		t.Errorf("Lang = %q", audio.Lang)
	}
}

func TestParseMuxedRepresentationNeedsNoMuxer(t *testing.T) {
	doc := `<MPD type="static" mediaPresentationDuration="PT10S"><Period>
	  <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f,mp4a.40.2">
	    <SegmentTemplate initialization="i.mp4" media="s-$Number$.mp4" duration="5" timescale="1"/>
	    <Representation id="m" width="854" height="480" bandwidth="900000"/>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://x/d/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := m.Video()[0]
	if !v.Muxed {
		t.Fatal("a representation with both codecs is muxed")
	}
	if m.NeedsMuxing(v) {
		t.Fatal("a muxed representation needs no muxer")
	}
	if len(v.Segments) != 2 || v.Segments[0] != "https://x/d/s-1.mp4" {
		t.Fatalf("segments = %v", v.Segments)
	}
}

func TestParseSingleFileRepresentation(t *testing.T) {
	doc := `<MPD type="static" mediaPresentationDuration="PT30S"><Period>
	  <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f,mp4a.40.2">
	    <Representation id="f" width="1280" height="720" bandwidth="1500000">
	      <BaseURL>video-720.mp4</BaseURL>
	      <SegmentBase indexRange="0-1200"><Initialization range="0-800"/></SegmentBase>
	    </Representation>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://x/dash/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := m.Video()[0]
	if v.File != "https://x/dash/video-720.mp4" {
		t.Fatalf("File = %q", v.File)
	}
	if len(v.Segments) != 0 {
		t.Errorf("a single file needs no segment list: %v", v.Segments)
	}
}

func TestParseSegmentList(t *testing.T) {
	doc := `<MPD type="static"><Period duration="PT8S">
	  <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f,mp4a.40.2">
	    <SegmentList timescale="1000">
	      <Initialization sourceURL="init.mp4"/>
	      <SegmentURL media="s1.mp4"/><SegmentURL media="s2.mp4"/><SegmentURL/>
	    </SegmentList>
	    <Representation id="l" height="480" bandwidth="800000"/>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://x/d/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := m.Video()[0]
	if v.Init != "https://x/d/init.mp4" || len(v.Segments) != 2 {
		t.Fatalf("stream = %+v", v)
	}
}

func TestParseBaseURLInheritance(t *testing.T) {
	doc := `<MPD type="static" mediaPresentationDuration="PT4S"><BaseURL>https://cdn.example/a/</BaseURL>
	  <Period duration="PT4S"><BaseURL>b/</BaseURL>
	    <AdaptationSet mimeType="video/mp4" codecs="avc1,mp4a"><BaseURL>c/</BaseURL>
	      <SegmentTemplate media="$Number$.m4s" duration="4" timescale="1"/>
	      <Representation id="r" height="360" bandwidth="1"/>
	    </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://site.example/x.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := m.Video()[0].Segments[0]; got != "https://cdn.example/a/b/c/1.m4s" {
		t.Fatalf("segment = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"not xml":       `{"a":1}`,
		"wrong root":    `<Playlist/>`,
		"no usable rep": `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4"><Representation id="x"/></AdaptationSet></Period></MPD>`,
		"template without media": `<MPD type="static"><Period duration="PT4S"><AdaptationSet mimeType="video/mp4">` +
			`<SegmentTemplate duration="4" timescale="1"/><Representation id="x" height="1"/></AdaptationSet></Period></MPD>`,
		"timeline without duration": `<MPD type="static"><Period duration="PT4S"><AdaptationSet mimeType="video/mp4">` +
			`<SegmentTemplate media="$Number$.m4s"><SegmentTimeline><S t="0"/></SegmentTimeline></SegmentTemplate>` +
			`<Representation id="x" height="1"/></AdaptationSet></Period></MPD>`,
		"no duration to count with": `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4">` +
			`<SegmentTemplate media="$Number$.m4s" duration="4" timescale="1"/><Representation id="x" height="1"/></AdaptationSet></Period></MPD>`,
	}
	for name, doc := range cases {
		if _, err := Parse(base(t, "https://x/m.mpd"), []byte(doc)); !errors.Is(err, ErrParse) {
			t.Errorf("%s: err = %v, want ErrParse", name, err)
		}
	}
}

func TestParseDynamicIsReported(t *testing.T) {
	doc := strings.Replace(templateManifest, `type="static"`, `type="dynamic"`, 1)
	m, err := Parse(base(t, "https://x/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.Dynamic {
		t.Fatal("a dynamic manifest must say so, so the caller can refuse it")
	}
}

func TestKindDetection(t *testing.T) {
	cases := []struct {
		mime, ctype, codecs string
		want                Kind
	}{
		{"video/mp4", "", "", KindVideo},
		{"audio/mp4", "", "", KindAudio},
		{"text/vtt", "", "", KindText},
		{"application/ttml+xml", "", "", KindText},
		{"", "video", "", KindVideo},
		{"", "", "mp4a.40.2", KindAudio},
		{"", "", "avc1.4d401f", KindVideo},
		{"application/mp4", "", "", KindVideo},
	}
	for _, c := range cases {
		if got := kindOf(c.mime, c.ctype, c.codecs); got != c.want {
			t.Errorf("kindOf(%q, %q, %q) = %q, want %q", c.mime, c.ctype, c.codecs, got, c.want)
		}
	}
}

func TestExpandPlaceholders(t *testing.T) {
	vars := map[string]string{"RepresentationID": "v1", "Bandwidth": "2400"}
	cases := map[string]string{
		"$RepresentationID$/seg-$Number$.m4s": "v1/seg-7.m4s",
		"$Bandwidth$/$Time$.m4s":              "2400/1234.m4s",
		"seg-$Number%05d$.m4s":                "seg-00007.m4s",
		"100$$percent-$Unknown$.m4s":          "100$percent-.m4s",
		"no placeholder":                      "no placeholder",
		"unterminated $Number":                "unterminated $Number",
	}
	for in, want := range cases {
		if got := expand(in, vars, 7, 1234); got != want {
			t.Errorf("expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT1M40S":  100 * time.Second,
		"PT1H2M3S": time.Hour + 2*time.Minute + 3*time.Second,
		"P1DT2H":   26 * time.Hour,
		"PT0.5S":   500 * time.Millisecond,
		"":         0,
		"1M40S":    0,
		"PT":       0,
		"PTxS":     0,
		"PT10Z":    0,
	}
	for in, want := range cases {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCodecHelpers(t *testing.T) {
	if !isMuxed("avc1.4d401f,mp4a.40.2") {
		t.Error("both codecs means muxed")
	}
	for _, c := range []string{"avc1.4d401f", "mp4a.40.2", "", "wvtt"} {
		if isMuxed(c) {
			t.Errorf("isMuxed(%q) = true", c)
		}
	}
	if !hasVideoCodec("av01.0.05M.08") || !hasAudioCodec("opus") {
		t.Error("modern codecs must be recognised")
	}
}

func TestVideoOrderingPrefersHeightThenBandwidth(t *testing.T) {
	m := &Manifest{Streams: []Stream{
		{Kind: KindVideo, Height: 720, Bandwidth: 1000},
		{Kind: KindVideo, Height: 1080, Bandwidth: 3000},
		{Kind: KindVideo, Height: 1080, Bandwidth: 6000},
		{Kind: KindAudio},
	}}
	got := m.Video()
	if len(got) != 3 || got[0].Bandwidth != 6000 || got[1].Bandwidth != 3000 || got[2].Height != 720 {
		t.Fatalf("Video() = %+v", got)
	}
	if m.NeedsMuxing(Stream{Kind: KindAudio}) {
		t.Error("an audio stream does not itself need muxing")
	}
	noAudio := &Manifest{Streams: []Stream{{Kind: KindVideo, Height: 720}}}
	if noAudio.NeedsMuxing(noAudio.Streams[0]) {
		t.Error("without a separate audio track there is nothing to mux")
	}
}

// TestURLHelpers covers how a manifest's own bases and its segment references
// are combined: a wrong answer here does not fail loudly, it fetches the wrong
// bytes from a plausible URL.
func TestURLHelpers(t *testing.T) {
	must := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	t.Run("resolveBase", func(t *testing.T) {
		manifest := must("https://cdn.example/a/b/stream.mpd")
		cases := []struct {
			name string
			base *url.URL
			refs []string
			want string
		}{
			{"no reference keeps the manifest", manifest, nil, "https://cdn.example/a/b/stream.mpd"},
			{"relative walks down", manifest, []string{"v/"}, "https://cdn.example/a/b/v/"},
			{"each reference resolves against the last", manifest,
				[]string{"v/", "hi/"}, "https://cdn.example/a/b/v/hi/"},
			{"absolute replaces", manifest,
				[]string{"https://other.example/x/"}, "https://other.example/x/"},
			{"blank is trimmed away", manifest, []string{"  "}, "https://cdn.example/a/b/stream.mpd"},
			{"an unreadable reference is passed over", manifest,
				[]string{"://nonsense", "v/"}, "https://cdn.example/a/b/v/"},
			{"with no manifest URL the first reference becomes the base", nil,
				[]string{"https://cdn.example/a/", "v/"}, "https://cdn.example/a/v/"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := resolveBase(tc.base, tc.refs)
				if got == nil || got.String() != tc.want {
					t.Fatalf("resolveBase = %v, want %s", got, tc.want)
				}
			})
		}
		// Every reference unreadable and no base at all: nothing to resolve
		// against, and that is said by returning nothing.
		if got := resolveBase(nil, []string{"://nonsense"}); got != nil {
			t.Fatalf("resolveBase = %v, want nothing", got)
		}
	})
	t.Run("join", func(t *testing.T) {
		base := must("https://cdn.example/a/v/")
		cases := []struct{ name, ref, want string }{
			{"relative", "seg-1.m4s", "https://cdn.example/a/v/seg-1.m4s"},
			{"rooted", "/other/seg.m4s", "https://cdn.example/other/seg.m4s"},
			{"already absolute", "https://x.example/s.m4s", "https://x.example/s.m4s"},
			{"query kept", "seg.m4s?t=1", "https://cdn.example/a/v/seg.m4s?t=1"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := join(base, tc.ref); got != tc.want {
					t.Fatalf("join = %q, want %q", got, tc.want)
				}
			})
		}
		// A reference that cannot be parsed is handed back as it came: the
		// manifest said it, and refusing to fetch it is the engine's call.
		if got := join(base, "://nonsense"); got != "://nonsense" {
			t.Errorf("join = %q, want the reference itself", got)
		}
		// With no base, only an absolute reference can be used.
		if got := join(nil, "https://x.example/s.m4s"); got != "https://x.example/s.m4s" {
			t.Errorf("join = %q", got)
		}
	})
}

// A well-formed XML document whose root is not <MPD> must be rejected by the
// explicit root check, not silently accepted. The error names the root it saw.
func TestParseRejectsNonMPDRoot(t *testing.T) {
	_, err := Parse(base(t, "https://x/m.mpd"), []byte(`<Manifest version="1"/>`))
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v, want ErrParse", err)
	}
	if !strings.Contains(err.Error(), `root element is "Manifest"`) {
		t.Fatalf("err = %q, want it to name the non-MPD root", err)
	}
}

// A SegmentTemplate whose SegmentTimeline is present but empty stands for no
// segment, so that representation yields nothing and is dropped. The precondition
// is that the template is otherwise valid (its media pattern works), proven by a
// sibling representation built from the same kind of template surviving.
func TestParseDropsRepresentationWithEmptyTimeline(t *testing.T) {
	doc := `<MPD type="static" mediaPresentationDuration="PT8S"><Period>
	  <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f">
	    <Representation id="good" height="720" bandwidth="900000">
	      <SegmentTemplate media="g-$Number$.m4s" duration="4" timescale="1"/>
	    </Representation>
	    <Representation id="empty" height="480" bandwidth="500000">
	      <SegmentTemplate media="e-$Number$.m4s" timescale="1"><SegmentTimeline></SegmentTimeline></SegmentTemplate>
	    </Representation>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://x/d/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("got %d streams, want only the good one", len(m.Streams))
	}
	if m.Streams[0].ID != "good" {
		t.Fatalf("surviving stream = %q, want the one with a non-empty timeline", m.Streams[0].ID)
	}
	if len(m.Streams[0].Segments) != 2 {
		t.Fatalf("good stream segments = %v, want 2", m.Streams[0].Segments)
	}
}

// A SegmentList whose SegmentURL entries carry no media stands for no segment,
// so its representation is dropped. A sibling list with real media survives,
// proving the list machinery itself works and it was the empty one that failed.
func TestParseDropsRepresentationWithEmptySegmentList(t *testing.T) {
	doc := `<MPD type="static"><Period duration="PT8S">
	  <AdaptationSet mimeType="video/mp4" codecs="avc1.4d401f">
	    <Representation id="good" height="720" bandwidth="900000">
	      <SegmentList><Initialization sourceURL="i.mp4"/><SegmentURL media="s1.mp4"/></SegmentList>
	    </Representation>
	    <Representation id="empty" height="480" bandwidth="500000">
	      <SegmentList><SegmentURL/><SegmentURL/></SegmentList>
	    </Representation>
	  </AdaptationSet></Period></MPD>`
	m, err := Parse(base(t, "https://x/d/m.mpd"), []byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Streams) != 1 {
		t.Fatalf("got %d streams, want only the good one", len(m.Streams))
	}
	if m.Streams[0].ID != "good" || len(m.Streams[0].Segments) != 1 {
		t.Fatalf("surviving stream = %+v, want id=good with 1 segment", m.Streams[0])
	}
}

// A duration whose numeric field reaches an allowed unit but does not parse as a
// float (here a lone ".") is malformed: parseDuration reports zero.
func TestParseDurationRejectsUnparseableNumber(t *testing.T) {
	if got := parseDuration("P.D"); got != 0 {
		t.Fatalf(`parseDuration("P.D") = %v, want 0 (the "." is not a number)`, got)
	}
}

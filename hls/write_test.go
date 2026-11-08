// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package hls

import (
	"bytes"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-streamkit/streamkit/internal/safe"
)

// The oracle is ffmpeg's hls muxer (-hls_segment_type fmp4), run on the same
// source as go-avkit's container/testdata/segment; its output is committed in
// testdata/ffmpeg.

func oracle(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "ffmpeg", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// tags are a playlist's lines up to the colon.
func tags(lines []string) []string {
	var out []string
	for _, l := range lines {
		if name, _, ok := strings.Cut(l, ":"); ok && strings.HasPrefix(l, "#") {
			out = append(out, name)
		} else {
			out = append(out, l)
		}
	}
	return out
}

// sameLines compares two playlists as sets of lines: RFC 8216 lets the header
// tags of a media playlist come in any order, and the two writers differ in it.
func sameLines(t *testing.T, got string, want []string) {
	t.Helper()
	g := strings.Split(strings.TrimSpace(got), "\n")
	w := slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("playlist differs from ffmpeg's:\n%s\nffmpeg:\n%s", got, strings.Join(want, "\n"))
	}
}

// sameAttrs checks every attribute ffmpeg states on a tag line is stated, with
// the same value, on the one at the same place in got.
func sameAttrs(t *testing.T, got string, want []string) {
	t.Helper()
	g := strings.Split(strings.TrimSpace(got), "\n")
	if len(g) != len(want) {
		t.Fatalf("%d lines, ffmpeg has %d:\n%s", len(g), len(want), got)
	}
	for i := range want {
		wt, wa, _ := strings.Cut(want[i], ":")
		gt, ga, _ := strings.Cut(g[i], ":")
		if wt != gt {
			t.Errorf("line %d: %q, ffmpeg %q", i+1, g[i], want[i])
			continue
		}
		if !strings.HasPrefix(wt, "#") {
			continue
		}
		gm, wm := parseAttrs(ga), parseAttrs(wa)
		for k, v := range wm {
			if gm[k] != v {
				t.Errorf("line %d: %s=%q, ffmpeg %q", i+1, k, gm[k], v)
			}
		}
	}
}

func TestAMediaPlaylistCarriesTheTagsFFmpegWritesForTheSameSegments(t *testing.T) {
	// ffmpeg's media_video.m3u8: four 2 s segments of init_video.mp4.
	m := MediaPlaylist{InitURI: "init_video.mp4", IndependentSegments: true}
	for _, n := range []string{"000", "001", "002", "003"} {
		m.Segments = append(m.Segments, MediaSegment{URI: "seg_video_" + n + ".m4s", Duration: 2})
	}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	sameLines(t, buf.String(), oracle(t, "hls/media_video.m3u8"))
}

func TestAnAudioPlaylistWithUnevenSegmentsRoundsItsTargetLikeFFmpeg(t *testing.T) {
	// media_audio.m3u8: 2.005333 s segments and a 21 ms one; TARGETDURATION:2,
	// no INDEPENDENT-SEGMENTS (ffmpeg writes it for video only).
	m := MediaPlaylist{InitURI: "init_audio.mp4"}
	for i, d := range []float64{2.005333, 2.005333, 2.005333, 1.984, 0.021333} {
		m.Segments = append(m.Segments, MediaSegment{URI: "seg_audio_00" + string(rune('0'+i)) + ".m4s", Duration: d})
	}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	sameLines(t, buf.String(), oracle(t, "hls/media_audio.m3u8"))
}

func TestAWebVTTPlaylistHasNoMapAndOldVersion(t *testing.T) {
	m := MediaPlaylist{Segments: []MediaSegment{{URI: "text1_00001.vtt", Duration: 2}, {URI: "text1_00002.vtt", Duration: 2}}}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	// ffmpeg's media_0_vtt.m3u8 has no PLAYLIST-TYPE and a version of 7; the
	// RFC needs 3 for decimal durations and the VOD type states the end.
	want := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:2\n" +
		"#EXTINF:2.000000,\ntext1_00001.vtt\n#EXTINF:2.000000,\ntext1_00002.vtt\n#EXT-X-ENDLIST\n"
	if buf.String() != want {
		t.Errorf("%q", buf.String())
	}
	ref := tags(oracle(t, "hls-subs/media_0_vtt.m3u8"))
	if len(ref) == 0 || ref[0] != "#EXTM3U" || ref[len(ref)-1] != "#EXT-X-ENDLIST" {
		t.Errorf("oracle %v", ref)
	}
}

func TestAMasterPlaylistCarriesTheTagsFFmpegWritesForTheSameRenditions(t *testing.T) {
	m := MasterPlaylist{
		Variants: []StreamInf{{
			URI: "media_video.m3u8", Bandwidth: 67208, AverageBandwidth: 66521, Width: 160, Height: 120,
			Codecs: []string{"avc1.4d400b", "mp4a.40.2"}, Audio: "group_aud",
		}},
		Renditions: []Rendition{{Type: "AUDIO", GroupID: "group_aud", Name: "audio_1", Default: true, Channels: 1, URI: "media_audio.m3u8"}},
	}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	// The attributes ffmpeg states are stated with its values; the order of the
	// attributes within a tag is free (RFC 8216 4.2), and ours also says DEFAULT=NO.
	sameAttrs(t, buf.String(), oracle(t, "hls/master.m3u8"))
}

func TestAMasterPlaylistNamesItsSubtitleGroup(t *testing.T) {
	m := MasterPlaylist{
		IndependentSegments: true,
		Variants: []StreamInf{{
			URI: "video1.m3u8", Bandwidth: 135535, Width: 160, Height: 120, FrameRate: 25,
			Codecs: []string{"avc1.4d400b", "mp4a.40.2"}, Audio: "aud", Subtitles: "subs",
		}},
		Renditions: []Rendition{
			{Type: "AUDIO", GroupID: "aud", Name: "English", Language: "en", URI: "audio1.m3u8", Default: true},
			{Type: "SUBTITLES", GroupID: "subs", Name: "English", Language: "en", URI: "text1.m3u8", Autoselect: true},
		},
	}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"#EXT-X-INDEPENDENT-SEGMENTS",
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio1.m3u8"`,
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=NO,AUTOSELECT=YES,URI="text1.m3u8"`,
		`BANDWIDTH=135535`, `RESOLUTION=160x120`, `FRAME-RATE=25.000`, `CODECS="avc1.4d400b,mp4a.40.2"`,
		`AUDIO="aud"`, `SUBTITLES="subs"`, "video1.m3u8",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// What ffmpeg's own subtitle master states: the SUBTITLES group and URI.
	ref := strings.Join(oracle(t, "hls-subs/master.m3u8"), "\n")
	if !strings.Contains(ref, "TYPE=SUBTITLES") || !strings.Contains(ref, `SUBTITLES="subs"`) {
		t.Errorf("oracle: %s", ref)
	}
}

func TestPlaylistsAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	ok := MediaSegment{URI: "a.m4s", Duration: 2}
	bad := []struct {
		name string
		m    MediaPlaylist
	}{
		{"no segment", MediaPlaylist{}},
		{"zero duration", MediaPlaylist{Segments: []MediaSegment{{URI: "a", Duration: 0}}}},
		{"NaN", MediaPlaylist{Segments: []MediaSegment{{URI: "a", Duration: math.NaN()}}}},
		{"infinite", MediaPlaylist{Segments: []MediaSegment{{URI: "a", Duration: math.Inf(1)}}}},
		{"traversal", MediaPlaylist{Segments: []MediaSegment{{URI: "../a", Duration: 1}}}},
		{"newline injects a tag", MediaPlaylist{Segments: []MediaSegment{{URI: "a\n#EXT-X-ENDLIST", Duration: 1}}}},
		{"init traversal", MediaPlaylist{InitURI: "/etc/passwd", Segments: []MediaSegment{ok}}},
		{"too many", MediaPlaylist{Segments: make([]MediaSegment, MaxWritten+1)}},
	}
	for _, c := range bad {
		var buf bytes.Buffer
		err := c.m.Write(&buf)
		if !errors.Is(err, ErrPlaylist) || buf.Len() != 0 {
			t.Errorf("%s: %v, %d bytes written", c.name, err, buf.Len())
		}
	}
	if err := (MediaPlaylist{Segments: []MediaSegment{{URI: "../a", Duration: 1}}}).Write(&bytes.Buffer{}); !errors.Is(err, safe.ErrUnsafe) {
		t.Errorf("the reason is lost: %v", err)
	}
}

func TestAMasterPlaylistRefusesWhatWouldBreakIt(t *testing.T) {
	v := StreamInf{URI: "v.m3u8", Bandwidth: 1}
	aud := Rendition{Type: "AUDIO", GroupID: "a", Name: "n", URI: "a.m3u8"}
	cases := []struct {
		name string
		m    MasterPlaylist
	}{
		{"no variant", MasterPlaylist{}},
		{"bad variant URI", MasterPlaylist{Variants: []StreamInf{{URI: "..", Bandwidth: 1}}}},
		{"no bandwidth", MasterPlaylist{Variants: []StreamInf{{URI: "v"}}}},
		{"half a resolution", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, Width: 5}}}},
		{"negative width", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, Width: -1, Height: -1}}}},
		{"NaN frame rate", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, FrameRate: math.NaN()}}}},
		{"infinite frame rate", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, FrameRate: math.Inf(1)}}}},
		{"negative frame rate", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, FrameRate: -1}}}},
		{"quote in codecs", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, Codecs: []string{`a"b`}}}}},
		{"undefined audio group", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, Audio: "zz"}}}},
		{"subtitles group of audio", MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1, Subtitles: "a"}}, Renditions: []Rendition{aud}}},
		{"bad type", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "VIDEO", GroupID: "a", Name: "n", URI: "a"}}}},
		{"quote in name", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a", Name: `n"`, URI: "a"}}}},
		{"newline in group", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a\nb", Name: "n", URI: "a"}}}},
		{"subtitles without language", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "SUBTITLES", GroupID: "s", Name: "n", URI: "s"}}}},
		{"quote in language", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "n", Language: `e"`, URI: "a"}}}},
		{"no rendition URI", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "n"}}}},
		{"rendition traversal", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "n", URI: "../x"}}}},
		{"channels", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{{Type: "AUDIO", GroupID: "a", Name: "n", URI: "a", Channels: 999}}}},
		{"mixed group", MasterPlaylist{Variants: []StreamInf{v}, Renditions: []Rendition{aud,
			{Type: "SUBTITLES", GroupID: "a", Name: "n", Language: "en", URI: "s"}}}},
		{"too many", MasterPlaylist{Variants: make([]StreamInf, MaxWritten+1)}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := c.m.Write(&buf); !errors.Is(err, ErrPlaylist) || buf.Len() != 0 {
			t.Errorf("%s: %v, %d bytes", c.name, err, buf.Len())
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteReportsTheWritersFailure(t *testing.T) {
	m := MediaPlaylist{Segments: []MediaSegment{{URI: "a", Duration: 1}}}
	if err := m.Write(failWriter{}); err == nil {
		t.Error("media")
	}
	ms := MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1}}}
	if err := ms.Write(failWriter{}); err == nil {
		t.Error("master")
	}
}

func TestAWrittenPlaylistParsesBackWithTheReader(t *testing.T) {
	m := MediaPlaylist{InitURI: "init video.mp4", Segments: []MediaSegment{{URI: "v_00001.m4s", Duration: 2.5}, {URI: "v 2.m4s", Duration: 1}}}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `#EXT-X-MAP:URI="init%20video.mp4"`) || !strings.Contains(buf.String(), "v%202.m4s") {
		t.Errorf("not escaped:\n%s", buf.String())
	}
	base, _ := url.Parse("https://cdn.example/p/media.m3u8")
	p, err := Parse(base, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if p.Live || p.Init == nil || p.Init.URL != "https://cdn.example/p/init%20video.mp4" || len(p.Segments) != 2 ||
		p.Segments[1].URL != "https://cdn.example/p/v%202.m4s" || p.TotalDuration() != 3.5 || p.TargetDuration != 3 {
		t.Errorf("%+v", p)
	}
}

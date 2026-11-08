// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package dash

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	empd "github.com/Eyevinn/dash-mpd/mpd"
)

// The oracle is ffmpeg's dash muxer, run on the same source as go-avkit's
// container/testdata/segment; its manifest is committed in testdata/ffmpeg/dash.

func oracleMPD(t *testing.T) *empd.MPD {
	t.Helper()
	m, err := empd.ReadFromFile(filepath.Join("..", "testdata", "ffmpeg", "dash", "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ffmpegPresentation is what ffmpeg's manifest says, as a Presentation.
func ffmpegPresentation() Presentation {
	video := Timeline([]Span{{0, 25600}, {25600, 25600}, {51200, 25600}, {76800, 25600}})
	audio := Timeline([]Span{{0, 92160}, {92160, 96256}, {188416, 96256}, {284672, 96256}, {380928, 3072}})
	return Presentation{
		Duration: 8 * time.Second, MinBufferTime: 4 * time.Second, MaxSegmentDuration: 2 * time.Second,
		AdaptationSets: []AdaptationSet{
			{ContentType: "video", MimeType: "video/mp4", Lang: "und", Representations: []Representation{{
				ID: "0", Codecs: "avc1.4d400b", Bandwidth: 64265, Width: 160, Height: 120, FrameRate: "25/1", SAR: "1:1",
				Timescale: 12800, InitURI: "init-$RepresentationID$.m4s",
				Media: "chunk-$RepresentationID$-$Number%05d$.m4s", Timeline: video,
			}}},
			{ContentType: "audio", MimeType: "audio/mp4", Lang: "und", Representations: []Representation{{
				ID: "1", Codecs: "mp4a.40.2", Bandwidth: 64278, AudioSamplingRate: 48000, AudioChannels: 1,
				Timescale: 48000, InitURI: "init-$RepresentationID$.m4s",
				Media: "chunk-$RepresentationID$-$Number%05d$.m4s", Timeline: audio,
			}}},
		},
	}
}

func TestTimelineMatchesTheSegmentTimelineFFmpegWrites(t *testing.T) {
	want := oracleMPD(t).Periods[0].AdaptationSets
	got := ffmpegPresentation().AdaptationSets
	for i := range want {
		wt := want[i].Representations[0].SegmentTemplate.SegmentTimeline.S
		gt := got[i].Representations[0].Timeline
		if len(wt) != len(gt) {
			t.Fatalf("set %d: %d S entries, ffmpeg %d", i, len(gt), len(wt))
		}
		for k := range wt {
			if gt[k].Duration != wt[k].D || gt[k].Repeat != wt[k].R || gt[k].HasStart != (wt[k].T != nil) ||
				(wt[k].T != nil && gt[k].Start != *wt[k].T) {
				t.Errorf("set %d S %d: %+v, ffmpeg d=%d r=%d t=%v", i, k, gt[k], wt[k].D, wt[k].R, wt[k].T)
			}
		}
	}
}

func TestAManifestCarriesTheAttributesFFmpegWrites(t *testing.T) {
	var buf bytes.Buffer
	if err := ffmpegPresentation().Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := empd.ReadFromString(buf.String())
	if err != nil {
		t.Fatal(err)
	}
	want := oracleMPD(t)
	if got.Profiles != want.Profiles || got.GetType() != want.GetType() ||
		*got.MediaPresentationDuration != *want.MediaPresentationDuration ||
		*got.MinBufferTime != *want.MinBufferTime || *got.MaxSegmentDuration != *want.MaxSegmentDuration {
		t.Errorf("MPD: profiles %q type %q duration %v buffer %v max %v; ffmpeg %q %q %v %v %v",
			got.Profiles, got.GetType(), *got.MediaPresentationDuration, *got.MinBufferTime, *got.MaxSegmentDuration,
			want.Profiles, want.GetType(), *want.MediaPresentationDuration, *want.MinBufferTime, *want.MaxSegmentDuration)
	}
	for i, ws := range want.Periods[0].AdaptationSets {
		gs := got.Periods[0].AdaptationSets[i]
		if gs.ContentType != ws.ContentType || gs.SegmentAlignment != ws.SegmentAlignment || gs.Lang != ws.Lang ||
			gs.StartWithSAP != ws.StartWithSAP {
			t.Errorf("set %d: %q align %v lang %q sap %d; ffmpeg %q %v %q %d", i, gs.ContentType, gs.SegmentAlignment,
				gs.Lang, gs.StartWithSAP, ws.ContentType, ws.SegmentAlignment, ws.Lang, ws.StartWithSAP)
		}
		wr, gr := ws.Representations[0], gs.Representations[0]
		if gr.Id != wr.Id || gr.MimeType != wr.MimeType || gr.Codecs != wr.Codecs || gr.Bandwidth != wr.Bandwidth ||
			gr.Width != wr.Width || gr.Height != wr.Height || gr.Sar != wr.Sar {
			t.Errorf("representation %d: %+v, ffmpeg %+v", i, gr.RepresentationBaseType, wr.RepresentationBaseType)
		}
		// ffmpeg states the frame rate on the adaptation set, this on the
		// representation (ISO/IEC 23009-1 lets either, through the common
		// attributes of RepresentationBase).
		if gr.FrameRate != ws.FrameRate {
			t.Errorf("frameRate %q, ffmpeg %q", gr.FrameRate, ws.FrameRate)
		}
		if (wr.AudioSamplingRate == nil) != (gr.AudioSamplingRate == nil) ||
			(wr.AudioSamplingRate != nil && *wr.AudioSamplingRate != *gr.AudioSamplingRate) {
			t.Errorf("representation %d: audioSamplingRate", i)
		}
		if len(wr.AudioChannelConfigurations) != len(gr.AudioChannelConfigurations) ||
			(len(wr.AudioChannelConfigurations) == 1 &&
				(wr.AudioChannelConfigurations[0].SchemeIdUri != gr.AudioChannelConfigurations[0].SchemeIdUri ||
					wr.AudioChannelConfigurations[0].Value != gr.AudioChannelConfigurations[0].Value)) {
			t.Errorf("representation %d: audio channel configuration", i)
		}
		wt, gt := wr.SegmentTemplate, gr.SegmentTemplate
		if *gt.Timescale != *wt.Timescale || gt.Initialization != wt.Initialization || gt.Media != wt.Media ||
			*gt.StartNumber != *wt.StartNumber || len(gt.SegmentTimeline.S) != len(wt.SegmentTimeline.S) {
			t.Errorf("template %d: %+v, ffmpeg %+v", i, gt, wt)
		}
	}
}

func TestAManifestReadsBackThroughTheParserWithItsSegments(t *testing.T) {
	var buf bytes.Buffer
	if err := ffmpegPresentation().Write(&buf); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://cdn.example/vod/manifest.mpd")
	m, err := Parse(base, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if m.Dynamic || m.Duration != 8*time.Second || len(m.Streams) != 2 {
		t.Fatalf("%+v", m)
	}
	v, a := m.Streams[0], m.Streams[1]
	if v.Kind != KindVideo || len(v.Segments) != 4 || v.Init != "https://cdn.example/vod/init-0.m4s" ||
		v.Segments[3] != "https://cdn.example/vod/chunk-0-00004.m4s" || v.Width != 160 {
		t.Errorf("video %+v", v)
	}
	if a.Kind != KindAudio || len(a.Segments) != 5 || a.Segments[4] != "https://cdn.example/vod/chunk-1-00005.m4s" {
		t.Errorf("audio %+v", a)
	}
}

func TestATextRepresentationHasNoInitialisationSegment(t *testing.T) {
	p := Presentation{Duration: 4 * time.Second, AdaptationSets: []AdaptationSet{{
		ContentType: "text", MimeType: "text/vtt", Lang: "en",
		Representations: []Representation{{
			ID: "text1", Bandwidth: 256, Timescale: 1000, StartNumber: 5, Media: "text1_$Number%05d$.vtt",
			Timeline: []TimelineEntry{{Duration: 2000, Repeat: 1, HasStart: true}},
		}},
	}}}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`minBufferTime="PT2S"`, `contentType="text"`, `startNumber="5"`, `media="text1_$Number%05d$.vtt"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "initialization") || strings.Contains(out, "maxSegmentDuration") {
		t.Errorf("unexpected attribute in\n%s", out)
	}
}

func TestTimelineStatesStartsWhereThereIsAGap(t *testing.T) {
	got := Timeline([]Span{{0, 10}, {10, 10}, {25, 10}, {35, 5}, {40, 5}})
	want := []TimelineEntry{
		{Start: 0, HasStart: true, Duration: 10, Repeat: 1},
		{Start: 25, HasStart: true, Duration: 10},
		{Start: 35, Duration: 5, Repeat: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if Timeline(nil) != nil {
		t.Error("nothing is not a timeline")
	}
}

func TestManifestsAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	rep := func(mod func(*Representation)) Presentation {
		r := Representation{ID: "v", Bandwidth: 1, Timescale: 1, Media: "v_$Number$.m4s", InitURI: "v_init.mp4",
			Timeline: []TimelineEntry{{Duration: 1}}}
		if mod != nil {
			mod(&r)
		}
		return Presentation{Duration: time.Second, AdaptationSets: []AdaptationSet{{
			ContentType: "video", MimeType: "video/mp4", Representations: []Representation{r}}}}
	}
	set := func(mod func(*AdaptationSet)) Presentation {
		p := rep(nil)
		mod(&p.AdaptationSets[0])
		return p
	}
	cases := []struct {
		name string
		p    Presentation
	}{
		{"no duration", Presentation{AdaptationSets: rep(nil).AdaptationSets}},
		{"negative buffer", func() Presentation { p := rep(nil); p.MinBufferTime = -1; return p }()},
		{"negative max segment", func() Presentation { p := rep(nil); p.MaxSegmentDuration = -1; return p }()},
		{"no set", Presentation{Duration: time.Second}},
		{"bad content type", set(func(a *AdaptationSet) { a.ContentType = "image" })},
		{"quote in mime", set(func(a *AdaptationSet) { a.MimeType = `a"` })},
		{"quote in lang", set(func(a *AdaptationSet) { a.Lang = `e"` })},
		{"no representation", set(func(a *AdaptationSet) { a.Representations = nil })},
		{"bad id", rep(func(r *Representation) { r.ID = "a b" })},
		{"quote in codecs", rep(func(r *Representation) { r.Codecs = `a"` })},
		{"quote in frame rate", rep(func(r *Representation) { r.FrameRate = `a"` })},
		{"no bandwidth", rep(func(r *Representation) { r.Bandwidth = 0 })},
		{"no timescale", rep(func(r *Representation) { r.Timescale = 0 })},
		{"negative width", rep(func(r *Representation) { r.Width = -1 })},
		{"template without number", rep(func(r *Representation) { r.Media = "v.m4s" })},
		{"no timeline", rep(func(r *Representation) { r.Timeline = nil })},
		{"traversing template", rep(func(r *Representation) { r.Media = "../$Number$.m4s" })},
		{"traversing init", rep(func(r *Representation) { r.InitURI = "/etc/x" })},
		{"zero duration entry", rep(func(r *Representation) { r.Timeline = []TimelineEntry{{}} })},
		{"negative repeat", rep(func(r *Representation) { r.Timeline = []TimelineEntry{{Duration: 1, Repeat: -1}} })},
		{"huge repeat", rep(func(r *Representation) { r.Timeline = []TimelineEntry{{Duration: 1, Repeat: MaxWritten + 1}} })},
		{"too many in a representation", rep(func(r *Representation) {
			r.Timeline = []TimelineEntry{{Duration: 1, Repeat: MaxWritten}, {Duration: 1}}
		})},
		{"duplicate id", func() Presentation {
			p := rep(nil)
			p.AdaptationSets = append(p.AdaptationSets, p.AdaptationSets[0])
			return p
		}()},
		{"too many in a manifest", func() Presentation {
			p := rep(func(r *Representation) { r.Timeline = []TimelineEntry{{Duration: 1, Repeat: MaxWritten - 1}} })
			other := p.AdaptationSets[0]
			other.Representations = []Representation{other.Representations[0]}
			other.Representations[0].ID = "w"
			other.Representations[0].Timeline = []TimelineEntry{{Duration: 1, Repeat: 1}}
			p.AdaptationSets = append(p.AdaptationSets, other)
			return p
		}()},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := c.p.Write(&buf); !errors.Is(err, ErrManifest) || buf.Len() != 0 {
			t.Errorf("%s: %v, %d bytes", c.name, err, buf.Len())
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteReportsTheWritersFailure(t *testing.T) {
	if err := ffmpegPresentation().Write(failWriter{}); err == nil {
		t.Error("no error")
	}
}

func TestTheTestdataIsTheOracle(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "testdata", "ffmpeg", "dash", "manifest.mpd")); err != nil {
		t.Fatal(err)
	}
}

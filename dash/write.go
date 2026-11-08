// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package dash

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	empd "github.com/Eyevinn/dash-mpd/mpd"

	"github.com/go-streamkit/streamkit/internal/safe"
)

// Writing a manifest wraps github.com/Eyevinn/dash-mpd (MIT), which holds the
// MPD schema; this file states what a static (VOD) presentation of CMAF/fMP4
// segments needs of it and refuses what it cannot be before anything is written.
//
// Sources: ISO/IEC 23009-1 5.3.9 (SegmentTemplate, SegmentTimeline: read through
// the dash-mpd types, which follow its schema; the standard itself is paywalled
// and was not opened), and the DASH-IF guidelines' timing model: a static MPD
// carries @type="static", the timescale of each template is required, segments
// of one adaptation set cover the same spans of the sample timeline, and no
// reference leaves a gap or an overlap. ffmpeg's dash muxer is the oracle.

// Profile is the profile a presentation of separate segment files, addressed by
// a template, states: ISO BMFF live, which ffmpeg writes for a static
// presentation too.
const Profile = "urn:mpeg:dash:profile:isoff-live:2011"

// DefaultMinBufferTime is the minBufferTime written when the caller states none.
const DefaultMinBufferTime = 2 * time.Second

// MaxWritten is the most segment references a representation, or representations
// in a manifest, may expand to.
const MaxWritten = 100000

// ErrManifest means the description cannot be a valid manifest, with the reason.
var ErrManifest = errors.New("dash: invalid manifest")

// Span is where a segment starts and how long it lasts, in the timescale of its
// representation.
type Span struct{ Start, Duration uint64 }

// TimelineEntry is one S element of a SegmentTimeline: Repeat more segments of
// the same duration follow the first, each starting where the last ended.
type TimelineEntry struct {
	// Start is the presentation time of the first segment (the t attribute),
	// written only when HasStart is set; otherwise the segment follows the
	// previous entry.
	Start    uint64
	HasStart bool
	Duration uint64
	Repeat   int
}

// Timeline collapses consecutive segments into S entries: runs of equal
// duration become one entry with a repeat count, and an entry states its start
// when it is the first or does not follow the previous segment (a gap or an
// overlap, which a player must be told of).
func Timeline(spans []Span) []TimelineEntry {
	var out []TimelineEntry
	var end uint64
	for i, s := range spans {
		last := len(out) - 1
		if i > 0 && s.Start == end && out[last].Duration == s.Duration {
			out[last].Repeat++
		} else {
			out = append(out, TimelineEntry{Start: s.Start, HasStart: i == 0 || s.Start != end, Duration: s.Duration})
		}
		end = s.Start + s.Duration
	}
	return out
}

// Representation is one encoding of a stream.
type Representation struct {
	// ID is letters, digits, underscore and dash.
	ID string
	// Codecs is the RFC 6381 string; empty for WebVTT files.
	Codecs    string
	Bandwidth uint32
	// Width, Height and FrameRate ("25/1") describe video.
	Width, Height int
	FrameRate     string
	// SAR is the sample aspect ratio, "1:1", or empty.
	SAR string
	// AudioSamplingRate and AudioChannels describe audio.
	AudioSamplingRate, AudioChannels int
	// Timescale is the unit of the timeline, required.
	Timescale uint32
	// StartNumber is the number of the first segment; 0 means 1.
	StartNumber uint32
	// InitURI is the template or file name of the initialisation segment,
	// empty for a representation that has none (WebVTT).
	InitURI string
	// Media is the segment template; it must hold $Number$ or $Time$.
	Media string
	// Timeline lists the segments.
	Timeline []TimelineEntry
}

// AdaptationSet groups the representations of one kind of content.
type AdaptationSet struct {
	// ContentType is "video", "audio" or "text".
	ContentType string
	// MimeType is "video/mp4", "audio/mp4" or "text/vtt".
	MimeType string
	// Lang is an RFC 5646 tag, or empty.
	Lang            string
	Representations []Representation
}

// Presentation is a static MPD to write.
type Presentation struct {
	// Duration is mediaPresentationDuration.
	Duration time.Duration
	// MinBufferTime defaults to DefaultMinBufferTime.
	MinBufferTime time.Duration
	// MaxSegmentDuration is written when set.
	MaxSegmentDuration time.Duration
	AdaptationSets     []AdaptationSet
}

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrManifest, fmt.Sprintf(format, args...))
}

// Write writes the manifest: a static MPD of one period, one adaptation set
// per entry flagged segmentAlignment and startWithSAP 1, and in each
// representation a SegmentTemplate with a SegmentTimeline. Templates and names
// are checked as the hls package checks URIs: relative, no "." or "..", escaped.
func (p Presentation) Write(w io.Writer) error {
	if p.Duration <= 0 || p.MinBufferTime < 0 || p.MaxSegmentDuration < 0 {
		return fail("duration %v, minBufferTime %v, maxSegmentDuration %v", p.Duration, p.MinBufferTime, p.MaxSegmentDuration)
	}
	if len(p.AdaptationSets) == 0 {
		return fail("no adaptation set")
	}
	m := empd.NewMPD(empd.STATIC_TYPE)
	m.Profiles = Profile
	m.MediaPresentationDuration = (*empd.Duration)(&p.Duration)
	buffer := p.MinBufferTime
	if buffer == 0 {
		buffer = DefaultMinBufferTime
	}
	m.MinBufferTime = (*empd.Duration)(&buffer)
	if p.MaxSegmentDuration > 0 {
		m.MaxSegmentDuration = (*empd.Duration)(&p.MaxSegmentDuration)
	}
	period := empd.NewPeriod()
	period.Id = "0"
	period.Start = empd.Seconds2DurPtr(0)
	ids := map[string]bool{}
	var references int
	for i, a := range p.AdaptationSets {
		set, n, err := buildAdaptationSet(uint32(i), a, ids)
		if err != nil {
			return fmt.Errorf("%w: adaptation set %d: %w", ErrManifest, i+1, err)
		}
		if references += n; references > MaxWritten {
			return fail("more than %d segment references", MaxWritten)
		}
		period.AppendAdaptationSet(set)
	}
	m.AppendPeriod(period)
	_, err := m.Write(w, "  ", true)
	return err
}

func buildAdaptationSet(id uint32, a AdaptationSet, ids map[string]bool) (*empd.AdaptationSetType, int, error) {
	if a.ContentType != "video" && a.ContentType != "audio" && a.ContentType != "text" {
		return nil, 0, fail("content type %q, want video, audio or text", a.ContentType)
	}
	if err := safe.Quoted("mimeType", a.MimeType); err != nil {
		return nil, 0, err
	}
	if a.Lang != "" {
		if err := safe.Quoted("lang", a.Lang); err != nil {
			return nil, 0, err
		}
	}
	if len(a.Representations) == 0 {
		return nil, 0, fail("no representation")
	}
	set := empd.NewAdaptationSetWithParams(a.ContentType, a.MimeType, true, 1)
	set.Id = empd.Ptr(id)
	set.Lang = a.Lang
	var references int
	for i, r := range a.Representations {
		rep, n, err := buildRepresentation(a, r)
		if err != nil {
			return nil, 0, fmt.Errorf("representation %d: %w", i+1, err)
		}
		if ids[r.ID] {
			return nil, 0, fail("the identifier %q is used twice", r.ID)
		}
		ids[r.ID] = true
		references += n
		set.AppendRepresentation(rep)
	}
	return set, references, nil
}

func buildRepresentation(a AdaptationSet, r Representation) (*empd.RepresentationType, int, error) {
	if err := safe.ID(r.ID); err != nil {
		return nil, 0, err
	}
	if r.Codecs != "" {
		if err := safe.Quoted("codecs", r.Codecs); err != nil {
			return nil, 0, err
		}
	}
	for _, f := range []struct{ what, v string }{{"frameRate", r.FrameRate}, {"sar", r.SAR}} {
		if f.v == "" {
			continue
		}
		if err := safe.Quoted(f.what, f.v); err != nil {
			return nil, 0, err
		}
	}
	switch {
	case r.Bandwidth == 0:
		return nil, 0, fail("bandwidth is required")
	case r.Timescale == 0:
		return nil, 0, fail("the timescale is required")
	case r.Width < 0 || r.Height < 0 || r.AudioSamplingRate < 0 || r.AudioChannels < 0:
		return nil, 0, fail("a negative size, rate or channel count")
	case !strings.Contains(r.Media, "$Number") && !strings.Contains(r.Media, "$Time"):
		return nil, 0, fail("the media template holds neither $Number$ nor $Time$")
	case len(r.Timeline) == 0:
		return nil, 0, fail("no segment")
	}
	media, err := safe.Template(r.Media)
	if err != nil {
		return nil, 0, err
	}
	rep := empd.NewRepresentationWithID(r.ID, r.Codecs, a.MimeType, int(r.Bandwidth))
	if r.Width > 0 {
		rep.Width, rep.Height = uint32(r.Width), uint32(r.Height)
		rep.FrameRate = empd.FrameRateType(r.FrameRate)
		rep.Sar = empd.RatioType(r.SAR)
	}
	if r.AudioSamplingRate > 0 {
		rate := empd.UIntVectorType(fmt.Sprint(r.AudioSamplingRate))
		rep.AudioSamplingRate = &rate
	}
	if r.AudioChannels > 0 {
		rep.AudioChannelConfigurations = append(rep.AudioChannelConfigurations, empd.NewDescriptor(
			"urn:mpeg:dash:23003:3:audio_channel_configuration:2011", fmt.Sprint(r.AudioChannels), ""))
	}
	st := empd.NewSegmentTemplate()
	st.Timescale = empd.Ptr(r.Timescale)
	st.Media = media
	if r.InitURI != "" {
		if st.Initialization, err = safe.Template(r.InitURI); err != nil {
			return nil, 0, fmt.Errorf("initialization: %w", err)
		}
	}
	start := r.StartNumber
	if start == 0 {
		start = 1
	}
	st.StartNumber = empd.Ptr(start)
	st.SegmentTimeline = empd.NewSegmentTimeline()
	var references int
	for i, e := range r.Timeline {
		if e.Duration == 0 || e.Repeat < 0 || e.Repeat > MaxWritten {
			return nil, 0, fail("timeline entry %d has the duration %d and the repeat %d", i+1, e.Duration, e.Repeat)
		}
		if references += 1 + e.Repeat; references > MaxWritten {
			return nil, 0, fail("more than %d segments in a representation", MaxWritten)
		}
		s := &empd.S{D: e.Duration, R: e.Repeat}
		if e.HasStart {
			s.T = empd.Ptr(e.Start)
		}
		st.SegmentTimeline.S = append(st.SegmentTimeline.S, s)
	}
	rep.SegmentTemplate = st
	return rep, references, nil
}

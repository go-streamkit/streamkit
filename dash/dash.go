// Copyright (c) 2026, the go-streamkit authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

// Package dash reads an MPEG-DASH manifest.
//
// Where HLS lists its segments, DASH computes them: a template names them with
// $Number$ or $Time$ placeholders and a timeline states their durations. And
// where the HLS these sites serve carries one muxed stream per rendition, DASH
// almost always keeps video and audio in separate representations, which a
// downloader cannot join without a muxer.
//
// So this package reports what a manifest really offers, including when a
// stream would need muxing: saying so is more useful than writing a file with
// no sound.
package dash

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Errors this package reports.
var (
	// ErrParse means the document is not a manifest this package can read.
	ErrParse = errors.New("dash: malformed manifest")
	// ErrLive means the manifest is dynamic: it describes a live edge that
	// keeps moving.
	ErrLive = errors.New("dash: live manifests are not supported")
)

// Kind is what a stream carries.
type Kind string

// The kinds a manifest distinguishes.
const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindText  Kind = "text"
)

// Stream is one representation, flattened with everything needed to fetch it.
type Stream struct {
	ID        string
	Kind      Kind
	MimeType  string
	Codecs    string
	Lang      string
	Width     int
	Height    int
	Bandwidth int
	// Muxed reports a stream that carries its own audio, which is the only
	// case a downloader can serve without a muxer.
	Muxed bool
	// File is set when the representation is one addressable file, which is
	// the case that needs nothing more than range requests.
	File string
	// Init and Segments describe a segmented representation.
	Init     string
	Segments []string
}

// Manifest is a parsed presentation.
type Manifest struct {
	Dynamic  bool
	Duration time.Duration
	Streams  []Stream
}

// HasAudio reports whether the presentation carries a separate audio stream,
// which is what makes a video-only stream unusable on its own.
func (m *Manifest) HasAudio() bool {
	for _, s := range m.Streams {
		if s.Kind == KindAudio {
			return true
		}
	}
	return false
}

// Video returns the video streams, best first.
func (m *Manifest) Video() []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.Kind == KindVideo {
			out = append(out, s)
		}
	}
	sortStreams(out)
	return out
}

// NeedsMuxing reports whether playing s also requires an audio stream that
// this downloader cannot join to it.
func (m *Manifest) NeedsMuxing(s Stream) bool {
	return s.Kind == KindVideo && !s.Muxed && m.HasAudio()
}

func sortStreams(s []Stream) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && better(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func better(a, b Stream) bool {
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	return a.Bandwidth > b.Bandwidth
}

// --- the XML shape --------------------------------------------------------

type mpd struct {
	XMLName                   xml.Name
	Type                      string   `xml:"type,attr"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr"`
	BaseURL                   []string `xml:"BaseURL"`
	Periods                   []period `xml:"Period"`
}

type period struct {
	Duration       string          `xml:"duration,attr"`
	BaseURL        []string        `xml:"BaseURL"`
	AdaptationSets []adaptationSet `xml:"AdaptationSet"`
}

type adaptationSet struct {
	MimeType        string           `xml:"mimeType,attr"`
	ContentType     string           `xml:"contentType,attr"`
	Lang            string           `xml:"lang,attr"`
	Codecs          string           `xml:"codecs,attr"`
	Width           int              `xml:"width,attr"`
	Height          int              `xml:"height,attr"`
	BaseURL         []string         `xml:"BaseURL"`
	SegmentTemplate *segmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *segmentList     `xml:"SegmentList"`
	Representations []representation `xml:"Representation"`
	ContentProtect  []struct {
		Scheme string `xml:"schemeIdUri,attr"`
	} `xml:"ContentProtection"`
}

type representation struct {
	ID              string           `xml:"id,attr"`
	MimeType        string           `xml:"mimeType,attr"`
	Codecs          string           `xml:"codecs,attr"`
	Width           int              `xml:"width,attr"`
	Height          int              `xml:"height,attr"`
	Bandwidth       int              `xml:"bandwidth,attr"`
	BaseURL         []string         `xml:"BaseURL"`
	SegmentBase     *segmentBase     `xml:"SegmentBase"`
	SegmentTemplate *segmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *segmentList     `xml:"SegmentList"`
}

type segmentBase struct {
	IndexRange     string `xml:"indexRange,attr"`
	Initialization *struct {
		Range string `xml:"range,attr"`
	} `xml:"Initialization"`
}

type segmentTemplate struct {
	Media          string           `xml:"media,attr"`
	Initialization string           `xml:"initialization,attr"`
	StartNumber    *int             `xml:"startNumber,attr"`
	Duration       int64            `xml:"duration,attr"`
	Timescale      int64            `xml:"timescale,attr"`
	Timeline       *segmentTimeline `xml:"SegmentTimeline"`
}

type segmentTimeline struct {
	Entries []struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R int64  `xml:"r,attr"`
	} `xml:"S"`
}

type segmentList struct {
	Timescale      int64 `xml:"timescale,attr"`
	Initialization *struct {
		SourceURL string `xml:"sourceURL,attr"`
	} `xml:"Initialization"`
	URLs []struct {
		Media string `xml:"media,attr"`
	} `xml:"SegmentURL"`
}

// Parse reads a manifest. base resolves the relative URLs it contains.
func Parse(base *url.URL, data []byte) (*Manifest, error) {
	var doc mpd
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if doc.XMLName.Local != "MPD" {
		return nil, fmt.Errorf("%w: root element is %q", ErrParse, doc.XMLName.Local)
	}
	out := &Manifest{Dynamic: strings.EqualFold(doc.Type, "dynamic")}
	out.Duration = parseDuration(doc.MediaPresentationDuration)

	mpdBase := resolveBase(base, doc.BaseURL)
	for _, p := range doc.Periods {
		periodBase := resolveBase(mpdBase, p.BaseURL)
		periodDuration := parseDuration(p.Duration)
		if periodDuration == 0 {
			periodDuration = out.Duration
		}
		for _, as := range p.AdaptationSets {
			asBase := resolveBase(periodBase, as.BaseURL)
			for _, rep := range as.Representations {
				s, ok := flatten(asBase, as, rep, periodDuration)
				if ok {
					out.Streams = append(out.Streams, s)
				}
			}
		}
	}
	if len(out.Streams) == 0 {
		return nil, fmt.Errorf("%w: no usable representation", ErrParse)
	}
	return out, nil
}

// flatten turns one representation into a Stream, inheriting what its
// adaptation set declares.
func flatten(base *url.URL, as adaptationSet, rep representation, periodDuration time.Duration) (Stream, bool) {
	mime := first(rep.MimeType, as.MimeType)
	codecs := first(rep.Codecs, as.Codecs)
	s := Stream{
		ID:        rep.ID,
		Kind:      kindOf(mime, as.ContentType, codecs),
		MimeType:  mime,
		Codecs:    codecs,
		Lang:      as.Lang,
		Width:     firstInt(rep.Width, as.Width),
		Height:    firstInt(rep.Height, as.Height),
		Bandwidth: rep.Bandwidth,
		Muxed:     isMuxed(codecs),
	}
	repBase := resolveBase(base, rep.BaseURL)
	// A representation names its own file through its own BaseURL. Falling
	// back to an inherited one would take the manifest itself for the media.
	ownFile := len(rep.BaseURL) > 0 && repBase != nil

	tmpl := firstTemplate(rep.SegmentTemplate, as.SegmentTemplate)
	list := firstList(rep.SegmentList, as.SegmentList)
	switch {
	case ownFile && (rep.SegmentBase != nil || (tmpl == nil && list == nil)):
		// One addressable file: range requests are all it takes.
		s.File = repBase.String()
	case tmpl != nil:
		init, segs, ok := fromTemplate(repBase, tmpl, rep, periodDuration)
		if !ok {
			return s, false
		}
		s.Init, s.Segments = init, segs
	case list != nil:
		init, segs, ok := fromList(repBase, list)
		if !ok {
			return s, false
		}
		s.Init, s.Segments = init, segs
	default:
		return s, false // nothing says where the media is
	}
	return s, true
}

// fromTemplate generates the segment URLs a template stands for.
func fromTemplate(base *url.URL, t *segmentTemplate, rep representation, periodDuration time.Duration) (string, []string, bool) {
	if t.Media == "" {
		return "", nil, false
	}
	vars := map[string]string{
		"RepresentationID": rep.ID,
		"Bandwidth":        strconv.Itoa(rep.Bandwidth),
	}
	init := ""
	if t.Initialization != "" {
		init = join(base, expand(t.Initialization, vars, 0, 0))
	}
	timescale := t.Timescale
	if timescale <= 0 {
		timescale = 1
	}
	start := 1
	if t.StartNumber != nil {
		start = *t.StartNumber
	}

	var segs []string
	switch {
	case t.Timeline != nil:
		number, tick := start, int64(0)
		for _, e := range t.Timeline.Entries {
			if e.T != nil {
				tick = *e.T
			}
			if e.D <= 0 {
				return "", nil, false
			}
			for i := int64(0); i <= e.R; i++ {
				segs = append(segs, join(base, expand(t.Media, vars, number, tick)))
				tick += e.D
				number++
			}
		}
	case t.Duration > 0 && periodDuration > 0:
		per := float64(t.Duration) / float64(timescale)
		count := int(periodDuration.Seconds()/per + 0.999999)
		for i := 0; i < count; i++ {
			segs = append(segs, join(base, expand(t.Media, vars, start+i, int64(i)*t.Duration)))
		}
	default:
		return "", nil, false
	}
	if len(segs) == 0 {
		return "", nil, false
	}
	return init, segs, true
}

// fromList reads the segments a manifest spells out.
func fromList(base *url.URL, l *segmentList) (string, []string, bool) {
	init := ""
	if l.Initialization != nil && l.Initialization.SourceURL != "" {
		init = join(base, l.Initialization.SourceURL)
	}
	var segs []string
	for _, u := range l.URLs {
		if u.Media == "" {
			continue
		}
		segs = append(segs, join(base, u.Media))
	}
	if len(segs) == 0 {
		return "", nil, false
	}
	return init, segs, true
}

// expand substitutes the placeholders of a segment template, honouring the
// printf-style width some manifests ask for, as in "$Number%05d$".
func expand(pattern string, vars map[string]string, number int, tick int64) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] != '$' {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		end := strings.IndexByte(pattern[i+1:], '$')
		if end < 0 {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		token := pattern[i+1 : i+1+end]
		i += end + 2
		if token == "" {
			b.WriteByte('$') // "$$" is an escaped dollar
			continue
		}
		name, format, _ := strings.Cut(token, "%")
		switch name {
		case "Number":
			b.WriteString(formatNumber(int64(number), format))
		case "Time":
			b.WriteString(formatNumber(tick, format))
		default:
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

// formatNumber renders n, padded when the template asked for it.
func formatNumber(n int64, format string) string {
	if format == "" {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprintf("%"+format, n)
}

// kindOf decides what a representation carries.
func kindOf(mime, contentType, codecs string) Kind {
	switch {
	case strings.HasPrefix(contentType, "video"), strings.HasPrefix(mime, "video/"):
		return KindVideo
	case strings.HasPrefix(contentType, "audio"), strings.HasPrefix(mime, "audio/"):
		return KindAudio
	case strings.HasPrefix(contentType, "text"), strings.HasPrefix(mime, "text/"),
		strings.HasPrefix(mime, "application/ttml"):
		return KindText
	}
	if hasVideoCodec(codecs) {
		return KindVideo
	}
	if hasAudioCodec(codecs) {
		return KindAudio
	}
	return KindVideo
}

// isMuxed reports a representation whose codec list names both a picture and a
// sound track.
func isMuxed(codecs string) bool {
	return hasVideoCodec(codecs) && hasAudioCodec(codecs)
}

func hasVideoCodec(codecs string) bool {
	return matchesAny(codecs, "avc1", "avc3", "hev1", "hvc1", "vp8", "vp9", "vp09", "av01", "mp4v")
}

func hasAudioCodec(codecs string) bool {
	return matchesAny(codecs, "mp4a", "ac-3", "ec-3", "opus", "vorbis", "alac", "flac")
}

func matchesAny(codecs string, names ...string) bool {
	low := strings.ToLower(codecs)
	for _, c := range strings.Split(low, ",") {
		c = strings.TrimSpace(c)
		for _, n := range names {
			if strings.HasPrefix(c, n) {
				return true
			}
		}
	}
	return false
}

// parseDuration reads the ISO 8601 duration a manifest states.
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || !strings.HasPrefix(s, "P") {
		return 0
	}
	body := s[1:]
	date, clock, _ := strings.Cut(body, "T")
	var total time.Duration
	units := map[byte]time.Duration{'D': 24 * time.Hour, 'H': time.Hour, 'M': time.Minute, 'S': time.Second}
	read := func(part string, allowed string) bool {
		num := strings.Builder{}
		for i := 0; i < len(part); i++ {
			c := part[i]
			switch {
			case (c >= '0' && c <= '9') || c == '.':
				num.WriteByte(c)
			case strings.IndexByte(allowed, c) >= 0:
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return false
				}
				total += time.Duration(v * float64(units[c]))
				num.Reset()
			default:
				return false
			}
		}
		return num.Len() == 0
	}
	if !read(date, "D") || !read(clock, "HMS") {
		return 0
	}
	return total
}

// --- URL helpers -----------------------------------------------------------

func resolveBase(base *url.URL, refs []string) *url.URL {
	out := base
	for _, ref := range refs {
		u, err := url.Parse(strings.TrimSpace(ref))
		if err != nil {
			continue
		}
		if out == nil {
			out = u
			continue
		}
		out = out.ResolveReference(u)
	}
	return out
}

func join(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if base == nil || u.IsAbs() {
		return u.String()
	}
	return base.ResolveReference(u).String()
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstTemplate(vals ...*segmentTemplate) *segmentTemplate {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstList(vals ...*segmentList) *segmentList {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

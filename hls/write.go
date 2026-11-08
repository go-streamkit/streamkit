// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package hls

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/go-streamkit/streamkit/internal/safe"
)

// Writing playlists wraps github.com/Eyevinn/hls-m3u8 (MIT), which encodes the
// tags; this file states what a VOD packaging of CMAF/fMP4 and WebVTT needs of
// it, and refuses what RFC 8216 does not allow before anything is written.
//
// Sources: RFC 8216 4.3.2.5 (EXT-X-MAP needs version 6 or later), 4.3.3.1
// (EXTINF rounded to the nearest integer stays within EXT-X-TARGETDURATION),
// 4.3.3.4 and 4.3.3.5 (EXT-X-ENDLIST, PLAYLIST-TYPE:VOD), 4.3.4.1 and 4.3.4.2
// (EXT-X-MEDIA, EXT-X-STREAM-INF), 4.3.5.1 (EXT-X-INDEPENDENT-SEGMENTS); Apple's
// HLS Authoring Specification 5.x (subtitles carry LANGUAGE) and 9.11.

// MaxWritten is the most segments, variants or renditions a playlist written by
// this package holds.
const MaxWritten = 100000

// Errors reported when writing a playlist.
var (
	// ErrPlaylist means the description cannot be a valid playlist, with the
	// reason.
	ErrPlaylist = errors.New("hls: invalid playlist")
)

// MediaSegment is one segment of a playlist to write.
type MediaSegment struct {
	// URI is a relative file reference (see Write for what is refused).
	URI string
	// Duration is in seconds.
	Duration float64
}

// MediaPlaylist is a VOD media playlist to write: fMP4 segments with the
// initialisation segment InitURI names, or WebVTT segments when InitURI is empty.
type MediaPlaylist struct {
	InitURI  string
	Segments []MediaSegment
	// IndependentSegments states that every segment starts with a random
	// access point (EXT-X-INDEPENDENT-SEGMENTS), which is so for video segments
	// cut at keyframes.
	IndependentSegments bool
	// Keys state the encryption of every segment and of the initialisation
	// segment: one EXT-X-KEY per entry, before the map. Only fMP4 (InitURI set)
	// is encrypted by this package; WebVTT is not.
	Keys []EncryptionKey
}

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPlaylist, fmt.Sprintf(format, args...))
}

// Write writes the playlist: EXT-X-VERSION, TARGETDURATION, MEDIA-SEQUENCE:0,
// PLAYLIST-TYPE:VOD, the map, one EXTINF and URI per segment, and ENDLIST. The
// target duration is the longest segment rounded to the nearest integer, as
// RFC 8216 4.3.3.1 has it, and the version is 7 for fMP4 (EXT-X-MAP needs 6) and
// 3 for WebVTT (decimal EXTINF durations need 3). Every URI must be a relative
// reference that stays in the playlist's directory: an absolute path, a scheme,
// a backslash or a "." or ".." segment is refused, and the rest is
// percent-escaped.
func (m MediaPlaylist) Write(w io.Writer) error {
	if len(m.Segments) == 0 || len(m.Segments) > MaxWritten {
		return fail("%d segments, want 1 to %d", len(m.Segments), MaxWritten)
	}
	keys, err := keyTags(m.Keys)
	if err != nil {
		return err
	}
	if len(keys) > 0 && m.InitURI == "" {
		return fail("keys on a playlist without an initialisation segment: only fMP4 is encrypted")
	}
	p, _ := m3u8.NewMediaPlaylist(0, uint(len(m.Segments))) // only an error when the window exceeds the capacity
	p.SetWritePrecision(6)
	var longest float64
	for i, s := range m.Segments {
		if !(s.Duration > 0) || math.IsInf(s.Duration, 0) {
			return fail("segment %d has the duration %v", i+1, s.Duration)
		}
		uri, err := safe.Reference(s.URI)
		if err != nil {
			return fmt.Errorf("%w: segment %d: %w", ErrPlaylist, i+1, err)
		}
		longest = max(longest, s.Duration)
		_ = p.Append(uri, s.Duration, "") // the capacity is the segment count
	}
	version := uint8(3)
	if m.InitURI != "" {
		uri, err := safe.Reference(m.InitURI)
		if err != nil {
			return fmt.Errorf("%w: initialisation segment: %w", ErrPlaylist, err)
		}
		p.SetDefaultMap(uri, 0, 0)
		version = 7
	}
	for _, k := range keys {
		_ = p.SetDefaultKey(k.Method, k.URI, k.IV, k.Keyformat, k.Keyformatversions) // never fails
	}
	p.SetVersion(version)
	p.SetTargetDuration(uint(max(math.Round(longest), 1)))
	p.MediaType = m3u8.VOD
	p.SetIndependentSegments(m.IndependentSegments)
	p.Close()
	_, err = w.Write(p.Encode().Bytes())
	return err
}

// Rendition is an alternative rendition of a master playlist: EXT-X-MEDIA.
type Rendition struct {
	// Type is "AUDIO" or "SUBTITLES".
	Type string
	// GroupID ties the rendition to the variants that name it.
	GroupID string
	// Name is what a player shows.
	Name string
	// Language is an RFC 5646 tag; required for SUBTITLES (Apple's HLS
	// Authoring Specification).
	Language string
	// URI is the media playlist of the rendition.
	URI string
	// Default marks the rendition a player picks without a choice made; it
	// implies Autoselect (RFC 8216 4.3.4.1).
	Default, Autoselect bool
	// Channels is the audio channel count; 0 states none.
	Channels int
}

// StreamInf is a variant of a master playlist: EXT-X-STREAM-INF and its URI.
type StreamInf struct {
	// URI is the media playlist of the variant.
	URI string
	// Bandwidth is the peak segment bit rate in bits per second, required.
	Bandwidth uint32
	// AverageBandwidth is the average, 0 to leave it out.
	AverageBandwidth uint32
	// Codecs are the RFC 6381 strings of everything the variant plays,
	// including its renditions' (RFC 8216 4.3.4.2 "SHOULD").
	Codecs []string
	// Width and Height state RESOLUTION; 0 leaves it out.
	Width, Height int
	// FrameRate states FRAME-RATE; 0 leaves it out.
	FrameRate float64
	// Audio and Subtitles name the GroupID of the renditions to play with.
	Audio, Subtitles string
}

// MasterPlaylist is a master (multivariant) playlist to write.
type MasterPlaylist struct {
	Variants   []StreamInf
	Renditions []Rendition
	// IndependentSegments states EXT-X-INDEPENDENT-SEGMENTS for every
	// segment of every playlist (Apple's HLS Authoring Specification 9.11).
	IndependentSegments bool
	// SessionKeys are EXT-X-SESSION-KEY tags (draft-pantos-hls-rfc8216bis
	// 4.4.6.5), which let a player preload the keys of the media playlists; each
	// must match the EXT-X-KEY of the playlists that use the same URI.
	SessionKeys []EncryptionKey
}

// Write writes the master playlist. Every URI is checked as MediaPlaylist.Write
// does, every name, language and group identifier must be fit for a quoted
// string, every variant names renditions that exist, and a default rendition is
// autoselected.
func (m MasterPlaylist) Write(w io.Writer) error {
	if len(m.Variants) == 0 || len(m.Variants) > MaxWritten || len(m.Renditions) > MaxWritten {
		return fail("%d variants and %d renditions", len(m.Variants), len(m.Renditions))
	}
	groups := map[string]string{} // GROUP-ID to TYPE
	var alts []*m3u8.Alternative
	for i, r := range m.Renditions {
		alt, err := rendition(r)
		if err != nil {
			return fmt.Errorf("%w: rendition %d: %w", ErrPlaylist, i+1, err)
		}
		if t, dup := groups[r.GroupID]; dup && t != r.Type {
			return fail("group %q holds both %s and %s renditions", r.GroupID, t, r.Type)
		}
		groups[r.GroupID] = r.Type
		alts = append(alts, alt)
	}
	session, err := keyTags(m.SessionKeys)
	if err != nil {
		return fmt.Errorf("session keys: %w", err)
	}
	p := m3u8.NewMasterPlaylist()
	for _, k := range session {
		k := k
		p.SessionKeys = append(p.SessionKeys, &k)
	}
	p.SetWritePrecision(3)
	p.SetVersion(7)
	p.SetIndependentSegments(m.IndependentSegments)
	for i, v := range m.Variants {
		params, uri, err := variant(v, groups)
		if err != nil {
			return fmt.Errorf("%w: variant %d: %w", ErrPlaylist, i+1, err)
		}
		if i == 0 {
			params.Alternatives = alts
		}
		p.Append(uri, nil, params)
	}
	_, err = w.Write(p.Encode().Bytes())
	return err
}

// rendition checks one rendition and turns it into its tag.
func rendition(r Rendition) (*m3u8.Alternative, error) {
	if r.Type != "AUDIO" && r.Type != "SUBTITLES" {
		return nil, fail("type %q, want AUDIO or SUBTITLES", r.Type)
	}
	for _, f := range []struct{ what, v string }{{"GROUP-ID", r.GroupID}, {"NAME", r.Name}} {
		if err := safe.Quoted(f.what, f.v); err != nil {
			return nil, err
		}
	}
	if r.Type == "SUBTITLES" && r.Language == "" {
		return nil, fail("a SUBTITLES rendition needs a language")
	}
	if r.Language != "" {
		if err := safe.Quoted("LANGUAGE", r.Language); err != nil {
			return nil, err
		}
	}
	if r.URI == "" {
		return nil, fail("a rendition needs the URI of its playlist")
	}
	uri, err := safe.Reference(r.URI)
	if err != nil {
		return nil, err
	}
	if r.Channels < 0 || r.Channels > 255 {
		return nil, fail("%d channels", r.Channels)
	}
	alt := &m3u8.Alternative{
		Type: r.Type, GroupId: r.GroupID, Name: r.Name, Language: r.Language, URI: uri,
		Default: r.Default, Autoselect: r.Autoselect || r.Default,
	}
	if r.Channels > 0 {
		alt.Channels = &m3u8.Channels{Amount: r.Channels}
	}
	return alt, nil
}

// variant checks one variant against the groups defined and turns it into its
// tag parameters.
func variant(v StreamInf, groups map[string]string) (m3u8.VariantParams, string, error) {
	var zero m3u8.VariantParams
	uri, err := safe.Reference(v.URI)
	if err != nil {
		return zero, "", err
	}
	if v.Bandwidth == 0 {
		return zero, "", fail("BANDWIDTH is required")
	}
	if v.Width < 0 || v.Height < 0 || (v.Width == 0) != (v.Height == 0) || math.IsNaN(v.FrameRate) ||
		math.IsInf(v.FrameRate, 0) || v.FrameRate < 0 {
		return zero, "", fail("resolution %dx%d, frame rate %v", v.Width, v.Height, v.FrameRate)
	}
	params := m3u8.VariantParams{
		Bandwidth: v.Bandwidth, AverageBandwidth: v.AverageBandwidth, FrameRate: v.FrameRate,
		Audio: v.Audio, Subtitles: v.Subtitles,
	}
	if v.Width > 0 {
		params.Resolution = fmt.Sprintf("%dx%d", v.Width, v.Height)
	}
	for i, c := range v.Codecs {
		if err := safe.Quoted("CODECS", c); err != nil {
			return zero, "", err
		}
		if i > 0 {
			params.Codecs += ","
		}
		params.Codecs += c
	}
	for _, g := range []struct{ want, id string }{{"AUDIO", v.Audio}, {"SUBTITLES", v.Subtitles}} {
		if g.id != "" && groups[g.id] != g.want {
			return zero, "", fail("%s group %q is not defined", g.want, g.id)
		}
	}
	return params, uri, nil
}

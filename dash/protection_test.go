// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package dash

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	empd "github.com/Eyevinn/dash-mpd/mpd"
)

// No packager's MPD with encryption is committed here: ffmpeg's dash muxer states
// none. The shape is that of the manifests in Eyevinn/dash-mpd's testdata (HbbTV
// manifest_wvcenc_1080p.mpd, DASH-IF livesim multi-drm.mpd), which a DASH player
// reads: one mp4protection element with the scheme in value and cenc:default_KID,
// then one element per DRM system.

const (
	testKID      = "43215678-1234-1234-1234-123412341236"
	widevineUUID = "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
)

func protectedPresentation(cps ...ContentProtection) Presentation {
	p := ffmpegPresentation()
	p.AdaptationSets[0].ContentProtection = cps
	return p
}

func TestAManifestStatesTheProtectionOfAnAdaptationSet(t *testing.T) {
	pssh := []byte{0, 0, 0, 12, 'p', 's', 's', 'h', 0, 0, 0, 0}
	var buf bytes.Buffer
	err := protectedPresentation(
		ContentProtection{SchemeIDURI: MP4ProtectionScheme, Value: "cenc", DefaultKID: testKID},
		ContentProtection{SchemeIDURI: widevineUUID, Value: "widevine", PSSH: pssh},
	).Write(&buf)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "urn:mpeg:cenc:2013") {
		t.Fatalf("the cenc namespace is not declared:\n%s", out)
	}
	m, err := empd.ReadFromString(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	set := m.Periods[0].AdaptationSets[0]
	if len(set.ContentProtections) != 2 || len(m.Periods[0].AdaptationSets[1].ContentProtections) != 0 {
		t.Fatalf("%d elements:\n%s", len(set.ContentProtections), out)
	}
	mp4p, drm := set.ContentProtections[0], set.ContentProtections[1]
	if string(mp4p.SchemeIdUri) != MP4ProtectionScheme || mp4p.Value != "cenc" || mp4p.DefaultKID != testKID {
		t.Errorf("mp4protection: %+v", mp4p)
	}
	if string(drm.SchemeIdUri) != widevineUUID || drm.Value != "widevine" || drm.Pssh == nil ||
		drm.Pssh.Value != base64.StdEncoding.EncodeToString(pssh) {
		t.Errorf("drm: %+v", drm)
	}
	// And the reader of this package still reads it.
	if _, err := Parse(nil, buf.Bytes()); err != nil {
		t.Errorf("Parse: %v", err)
	}
}

func TestProtectionIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	good := ContentProtection{SchemeIDURI: MP4ProtectionScheme, Value: "cbcs", DefaultKID: testKID}
	cases := map[string]ContentProtection{
		"noscheme":    {Value: "cenc"},
		"quote":       {SchemeIDURI: `a"b`},
		"valuequote":  {SchemeIDURI: widevineUUID, Value: "a\nb"},
		"mp4novalue":  {SchemeIDURI: MP4ProtectionScheme, DefaultKID: testKID},
		"mp4badvalue": {SchemeIDURI: MP4ProtectionScheme, Value: "aes", DefaultKID: testKID},
		"mp4nokid":    {SchemeIDURI: MP4ProtectionScheme, Value: "cenc"},
		"badkid":      {SchemeIDURI: widevineUUID, DefaultKID: "not-a-uuid"},
		"kidchar":     {SchemeIDURI: widevineUUID, DefaultKID: "4321567z-1234-1234-1234-123412341236"},
		"kiddash":     {SchemeIDURI: widevineUUID, DefaultKID: "43215678a1234-1234-1234-123412341236"},
		"bigpssh":     {SchemeIDURI: widevineUUID, PSSH: make([]byte, MaxPSSH+1)},
	}
	for name, c := range cases {
		var buf bytes.Buffer
		if err := protectedPresentation(good, c).Write(&buf); err == nil || buf.Len() != 0 {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	var buf bytes.Buffer
	err := protectedPresentation(make([]ContentProtection, MaxProtections+1)...).Write(&buf)
	if !errors.Is(err, ErrManifest) {
		t.Errorf("too many: %v", err)
	}
	// Both cases of hexadecimal digits are UUIDs.
	if !isUUID("AbCdEf01-2345-6789-abcd-EF0123456789") {
		t.Error("mixed case refused")
	}
}

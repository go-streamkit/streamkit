// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package hls

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/go-streamkit/streamkit/internal/safe"
)

// The oracle for the AES-128 tag is ffmpeg's hls muxer with -hls_key_info_file
// (testdata/ffmpeg/hls-key; it writes MPEG-TS, because "Encrypted fmp4 not yet
// supported" in ffmpeg 9.0.2, so SAMPLE-AES and SAMPLE-AES-CTR have no ffmpeg
// oracle: their tags follow draft-pantos-hls-rfc8216bis 4.4.4.4 as read).

var keyIV = []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

func keyedPlaylist(keys ...EncryptionKey) MediaPlaylist {
	return MediaPlaylist{InitURI: "init.mp4", Keys: keys,
		Segments: []MediaSegment{{URI: "a.m4s", Duration: 2}, {URI: "b.m4s", Duration: 2}}}
}

func writeMedia(t *testing.T, m MediaPlaylist) string {
	t.Helper()
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestAnAES128KeyTagIsTheOneFFmpegWrites(t *testing.T) {
	want := ""
	for _, l := range oracle(t, "hls-key/media_video.m3u8") {
		if strings.HasPrefix(l, "#EXT-X-KEY") {
			want = l
		}
	}
	got := writeMedia(t, keyedPlaylist(EncryptionKey{Method: MethodAES128, URI: "https://keys.example/key.bin", IV: keyIV}))
	if want == "" || !strings.Contains(got, want+"\n") {
		t.Errorf("want %q in:\n%s", want, got)
	}
}

func TestSampleEncryptionKeysAreWrittenBeforeTheMapAndReadBack(t *testing.T) {
	const widevine = "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	out := writeMedia(t, keyedPlaylist(
		EncryptionKey{Method: MethodSampleAESCTR, URI: "data:text/plain;base64,AAAA", Format: widevine, FormatVersions: "1"},
		EncryptionKey{Method: MethodSampleAES, URI: "skd://key", IV: keyIV, Format: "com.apple.streamingkeydelivery", FormatVersions: "1"},
	))
	for _, want := range []string{
		`#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,URI="data:text/plain;base64,AAAA",KEYFORMAT="` + widevine + `",KEYFORMATVERSIONS="1"`,
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key",IV=0x00112233445566778899aabbccddeeff,KEYFORMAT="com.apple.streamingkeydelivery"`,
		"#EXT-X-VERSION:7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// 4.4.4.4: the key applies to the EXT-X-MAP that follows it.
	if strings.Index(out, "#EXT-X-KEY") > strings.Index(out, "#EXT-X-MAP") {
		t.Errorf("the keys come after the map:\n%s", out)
	}
	base, _ := url.Parse("https://cdn.example/p/media.m3u8")
	p, err := Parse(base, []byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if k := p.Segments[0].Key; k == nil || k.Method == "" {
		t.Errorf("the reader sees no key: %+v", p.Segments[0])
	}
}

func TestAMasterPlaylistStatesSessionKeys(t *testing.T) {
	m := MasterPlaylist{
		Variants:    []StreamInf{{URI: "v.m3u8", Bandwidth: 1000}},
		SessionKeys: []EncryptionKey{{Method: MethodSampleAESCTR, URI: "https://lic.example/k", Format: "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"}},
	}
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	want := `#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES-CTR,URI="https://lic.example/k",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("missing %q in:\n%s", want, buf.String())
	}
	// Written twice, the same.
	var again bytes.Buffer
	_ = m.Write(&again)
	if again.String() != buf.String() {
		t.Error("not deterministic")
	}
}

func TestKeysAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	good := EncryptionKey{Method: MethodSampleAES, URI: "skd://k"}
	cases := map[string]EncryptionKey{
		"none":     {Method: "NONE", URI: "x"},
		"gcm":      {Method: "AES-256-GCM", URI: "x"},
		"nomethod": {URI: "x"},
		"nouri":    {Method: MethodSampleAES},
		"quote":    {Method: MethodSampleAES, URI: `a"b`},
		"newline":  {Method: MethodSampleAES, URI: "a\nb"},
		"ctriv":    {Method: MethodSampleAESCTR, URI: "x", IV: keyIV},
		"shortiv":  {Method: MethodSampleAES, URI: "x", IV: keyIV[:8]},
		"format":   {Method: MethodSampleAES, URI: "x", Format: "a\"b"},
		"versions": {Method: MethodSampleAES, URI: "x", FormatVersions: "1\n2"},
		"longuri":  {Method: MethodSampleAES, URI: strings.Repeat("a", 3000)},
	}
	for name, k := range cases {
		var buf bytes.Buffer
		if err := keyedPlaylist(k).Write(&buf); !errors.Is(err, ErrPlaylist) && !errors.Is(err, safe.ErrUnsafe) || buf.Len() != 0 {
			t.Errorf("media %s: %v (%d bytes)", name, err, buf.Len())
		}
		buf.Reset()
		ms := MasterPlaylist{Variants: []StreamInf{{URI: "v", Bandwidth: 1}}, SessionKeys: []EncryptionKey{k}}
		if err := ms.Write(&buf); err == nil || buf.Len() != 0 {
			t.Errorf("master %s: %v", name, err)
		}
	}
	// Too many keys, and keys on WebVTT.
	many := make([]EncryptionKey, MaxKeys+1)
	for i := range many {
		many[i] = good
	}
	if err := keyedPlaylist(many...).Write(&bytes.Buffer{}); !errors.Is(err, ErrPlaylist) {
		t.Errorf("many: %v", err)
	}
	vtt := keyedPlaylist(good)
	vtt.InitURI = ""
	if err := vtt.Write(&bytes.Buffer{}); !errors.Is(err, ErrPlaylist) {
		t.Errorf("webvtt: %v", err)
	}
}

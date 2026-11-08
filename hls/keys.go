// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package hls

import (
	"fmt"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"github.com/go-streamkit/streamkit/internal/safe"
)

// Encrypted playlists: EXT-X-KEY and EXT-X-SESSION-KEY.
//
// Sources: draft-pantos-hls-rfc8216bis 4.4.4.4 (EXT-X-KEY: METHOD is NONE,
// AES-128, SAMPLE-AES or SAMPLE-AES-CTR; URI is required unless the method is
// NONE; IV must not be present for SAMPLE-AES-CTR and needs compatibility version
// 2; KEYFORMAT and KEYFORMATVERSIONS need version 5; the tag applies to every
// media segment and every EXT-X-MAP initialisation section after it; with fMP4,
// SAMPLE-AES is the Common Encryption scheme cbcs and SAMPLE-AES-CTR is cenc) and
// 4.4.6.5 (EXT-X-SESSION-KEY: the attributes of EXT-X-KEY, METHOD not NONE; METHOD,
// KEYFORMAT and KEYFORMATVERSIONS must match any EXT-X-KEY with the same URI; SHOULD
// be used when several variants share keys). RFC 8216 4.3.2.4 for the first three
// methods.

// Key methods a playlist written by this package may state.
const (
	// MethodAES128 encrypts whole media segments (RFC 8216 4.3.2.4).
	MethodAES128 = "AES-128"
	// MethodSampleAES is, for fMP4, Common Encryption scheme cbcs.
	MethodSampleAES = "SAMPLE-AES"
	// MethodSampleAESCTR is, for fMP4, Common Encryption scheme cenc.
	MethodSampleAESCTR = "SAMPLE-AES-CTR"
)

// MaxKeys is the most keys a playlist written by this package states: one per
// KEYFORMAT of a multi-DRM packaging is the normal case.
const MaxKeys = 16

// EncryptionKey is an EXT-X-KEY (or EXT-X-SESSION-KEY) tag to write. The key
// itself is never held here: URI says where a player gets it, as a licence
// server's URL, an skd:// or data: URI, or a file.
type EncryptionKey struct {
	// Method is MethodAES128, MethodSampleAES or MethodSampleAESCTR.
	Method string
	// URI is required. It is not a file reference of the presentation: any
	// scheme is allowed, quotes and control characters are not.
	URI string
	// IV is the 16-byte initialization vector, or nil. SAMPLE-AES-CTR takes none.
	IV []byte
	// Format is KEYFORMAT (a DRM system, such as
	// "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"); empty means "identity".
	Format string
	// FormatVersions is KEYFORMATVERSIONS ("1", "1/2"); empty means 1.
	FormatVersions string
}

// tag checks the key and returns its tag.
func (k EncryptionKey) tag() (m3u8.Key, error) {
	var zero m3u8.Key
	switch k.Method {
	case MethodAES128, MethodSampleAES, MethodSampleAESCTR:
	default:
		return zero, fail("key method %q, want %s, %s or %s", k.Method, MethodAES128, MethodSampleAES, MethodSampleAESCTR)
	}
	if err := safe.Quoted("key URI", k.URI); err != nil {
		return zero, err
	}
	out := m3u8.Key{Method: k.Method, URI: k.URI, Keyformat: k.Format, Keyformatversions: k.FormatVersions}
	switch {
	case len(k.IV) == 0:
	case k.Method == MethodSampleAESCTR:
		return zero, fail("a SAMPLE-AES-CTR key takes no IV")
	case len(k.IV) != 16:
		return zero, fail("an IV is 16 bytes, got %d", len(k.IV))
	default:
		out.IV = fmt.Sprintf("0x%x", k.IV) // lower case, as ffmpeg writes it
	}
	for _, f := range []struct{ what, v string }{{"KEYFORMAT", k.Format}, {"KEYFORMATVERSIONS", k.FormatVersions}} {
		if f.v == "" {
			continue
		}
		if err := safe.Quoted(f.what, f.v); err != nil {
			return zero, err
		}
	}
	return out, nil
}

// keyTags checks a list of keys.
func keyTags(keys []EncryptionKey) ([]m3u8.Key, error) {
	if len(keys) > MaxKeys {
		return nil, fail("%d keys, at most %d", len(keys), MaxKeys)
	}
	out := make([]m3u8.Key, len(keys))
	for i, k := range keys {
		t, err := k.tag()
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i+1, err)
		}
		out[i] = t
	}
	return out, nil
}

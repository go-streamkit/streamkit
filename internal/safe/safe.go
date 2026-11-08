// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package safe validates what the playlist and manifest writers put in a file
// they produce: the URIs of segments, which are built from names and end up
// beside files written to a directory, and the quoted attribute values of HLS
// and the attributes of an MPD.
//
// A URI written into a playlist is refused when it could leave the directory the
// playlist sits in (absolute paths, "." and ".." segments, backslashes, a scheme)
// or when it could break the playlist's own syntax (control characters, which
// would let a name start a new tag line, and quotes, which would end an
// attribute value early). What is left is percent-escaped.
package safe

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnsafe means a value is refused, with the reason.
var ErrUnsafe = errors.New("safe: unsafe value")

// MaxLength is the longest URI or attribute value accepted.
const MaxLength = 2048

func unsafe(what, why string) error { return fmt.Errorf("%w: %s: %s", ErrUnsafe, what, why) }

// plain checks a string holds nothing a text format treats as structure: valid
// UTF-8, no control character, no line or paragraph separator.
func plain(what, s string) error {
	switch {
	case s == "":
		return unsafe(what, "empty")
	case len(s) > MaxLength:
		return unsafe(what, "too long")
	case !utf8.ValidString(s):
		return unsafe(what, "not valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return unsafe(what, "holds a control character")
		}
	}
	return nil
}

// Quoted checks a value that is written inside a quoted string: it must not
// hold a quote, a control character or a line break.
func Quoted(what, s string) error {
	if err := plain(what, s); err != nil {
		return err
	}
	if strings.ContainsRune(s, '"') {
		return unsafe(what, "holds a quote")
	}
	return nil
}

// Reference validates name as a relative file reference and returns it as a URI:
// each path segment percent-escaped, the separators kept. It refuses an absolute
// path, a scheme, a backslash, an empty segment, and "." or ".." as a segment.
func Reference(name string) (string, error) {
	if err := plain("URI", name); err != nil {
		return "", err
	}
	if strings.ContainsRune(name, '\\') {
		return "", unsafe("URI", "holds a backslash")
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "":
			return "", unsafe("URI", "is absolute or has an empty path segment")
		case ".", "..":
			return "", unsafe("URI", "has a . or .. path segment")
		}
	}
	return escape(name), nil
}

// escape percent-escapes each segment. A colon is escaped too: in the first
// segment of a relative reference it would read as a scheme (RFC 3986 section 4.2).
func escape(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(p), ":", "%3A")
	}
	return strings.Join(parts, "/")
}

// identifiers are the DASH SegmentTemplate identifiers (ISO/IEC 23009-1 5.3.9.4.4)
// and whether each takes a printf-style format tag.
var identifiers = map[string]bool{"RepresentationID": false, "Number": true, "Bandwidth": true, "Time": true}

// Template validates a DASH SegmentTemplate pattern: a relative reference in
// which $RepresentationID$, $Number$, $Bandwidth$ and $Time$ (the last three
// with an optional %0Nd tag) and $$ stand. The literal parts are escaped as
// Reference does; the identifiers are kept.
func Template(pattern string) (string, error) {
	if err := plain("template", pattern); err != nil {
		return "", err
	}
	parts := strings.Split(pattern, "$")
	if len(parts)%2 == 0 {
		return "", unsafe("template", "has an unbalanced $")
	}
	var probe strings.Builder // the pattern with every identifier read as a digit
	var out strings.Builder
	for i, p := range parts {
		if i%2 == 0 {
			probe.WriteString(p)
			out.WriteString(escape(p))
			continue
		}
		if p == "" { // $$, a literal dollar
			probe.WriteString("0")
			out.WriteString("$$")
			continue
		}
		if !identifier(p) {
			return "", unsafe("template", "holds the unknown identifier $"+p+"$")
		}
		probe.WriteString("0")
		out.WriteString("$" + p + "$")
	}
	if _, err := Reference(probe.String()); err != nil {
		return "", err
	}
	return out.String(), nil
}

// identifier reports whether p is an identifier, with its optional format tag.
func identifier(p string) bool {
	name, tag, hasTag := strings.Cut(p, "%")
	takesTag, known := identifiers[name]
	if !known {
		return false
	}
	if !hasTag {
		return true
	}
	if !takesTag || len(tag) < 3 || tag[0] != '0' || tag[len(tag)-1] != 'd' {
		return false
	}
	width := tag[1 : len(tag)-1]
	if len(width) > 2 {
		return false
	}
	for _, c := range width {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ID checks a Representation identifier, which ends up in a template's
// expansion and so in a file name: letters, digits, underscore and dash.
func ID(s string) error {
	if s == "" || len(s) > 64 {
		return unsafe("identifier", "empty or longer than 64 bytes")
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return unsafe("identifier", "holds a character other than a letter, digit, _ or -")
		}
	}
	return nil
}

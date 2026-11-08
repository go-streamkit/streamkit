// Copyright (c) the go-streamkit authors.
// SPDX-License-Identifier: BSD-3-Clause

package safe

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestReferenceEscapesWhatIsLeftAndRefusesTheRest(t *testing.T) {
	ok := map[string]string{
		"video1_00001.m4s": "video1_00001.m4s",
		"a b.m4s":          "a%20b.m4s",
		"sub/dir/é.m4s":    "sub/dir/%C3%A9.m4s",
		"a:b.m4s":          "a%3Ab.m4s",
		"q?x#y":            "q%3Fx%23y",
		".hidden":          ".hidden",
		"100%":             "100%25",
	}
	for in, want := range ok {
		got, err := Reference(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "/abs", "a//b", "a/", "..", "../a", "a/../b", ".", "./a", `a\b`, "a\nb", "a\x00b", "a\x7fb", "a b",
		"\xff", strings.Repeat("a", MaxLength+1),
	} {
		if _, err := Reference(in); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%q accepted: %v", in, err)
		}
	}
}

func TestQuotedRefusesWhatEndsAQuotedString(t *testing.T) {
	if err := Quoted("x", "English (US), 5.1"); err != nil {
		t.Error(err)
	}
	for _, in := range []string{"", `a"b`, "a\nb", "a\rb"} {
		if err := Quoted("x", in); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestTemplateKeepsIdentifiersAndChecksTheRest(t *testing.T) {
	ok := map[string]string{
		"chunk-$RepresentationID$-$Number%05d$.m4s": "chunk-$RepresentationID$-$Number%05d$.m4s",
		"$Time$.m4s":       "$Time$.m4s",
		"$Bandwidth%03d$/": "", // refused: trailing slash
		"a b_$Number$.m4s": "a%20b_$Number$.m4s",
		"price$$_$Number$": "price$$_$Number$",
	}
	for in, want := range ok {
		got, err := Template(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q accepted", in)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "a$b", "$Foo$", "$Number%5d$", "$Number%0d$", "$Number%0xd$", "$Number%0123d$", "$RepresentationID%05d$",
		"$Number%05$", "../$Number$", "/$Number$", "a\n$Number$", "$$$",
	} {
		if _, err := Template(in); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%q accepted: %v", in, err)
		}
	}
}

func TestIDIsLettersDigitsUnderscoreAndDash(t *testing.T) {
	for _, in := range []string{"v", "Video_1-b", "0"} {
		if err := ID(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	for _, in := range []string{"", "a b", "a/b", "é", strings.Repeat("a", 65)} {
		if err := ID(in); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%q accepted", in)
		}
	}
}

func FuzzReference(f *testing.F) {
	for _, s := range []string{"a/b.m4s", "../x", "a b", "a:b", "é\n", "//", "%2e%2e/x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Reference(s)
		if err != nil {
			return
		}
		// What is accepted, read back as a URL reference, stays a relative
		// path inside the directory: no scheme, no host, no leading slash, no
		// dot segment, and it unescapes to the input.
		u, perr := url.Parse(got)
		if perr != nil || u.Scheme != "" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasPrefix(got, "/") {
			t.Fatalf("%q became %q, which parses as %+v (%v)", s, got, u, perr)
		}
		back, uerr := url.PathUnescape(got)
		if uerr != nil || back != s {
			t.Fatalf("%q does not unescape from %q: %q %v", s, got, back, uerr)
		}
		for _, seg := range strings.Split(back, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q holds the segment %q", s, seg)
			}
		}
	})
}

func FuzzTemplate(f *testing.F) {
	for _, s := range []string{"c-$Number%05d$.m4s", "$$", "$Time$", "../$Number$", "a$"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Template(s)
		if err != nil {
			return
		}
		if strings.ContainsAny(got, "\n\r\x00\\") || strings.HasPrefix(got, "/") || strings.Contains(got, "..") && !strings.Contains(s, "..") {
			t.Fatalf("%q became %q", s, got)
		}
		if _, err := url.Parse(strings.ReplaceAll(got, "$", "0")); err != nil {
			t.Fatalf("%q became %q: %v", s, got, err)
		}
	})
}

# streamkit

Mutualized, site-agnostic transport code for streaming media, in pure Go.

`streamkit` gathers three small, self-contained packages that any streaming
client needs and that carry no knowledge of where their bytes come from: a
resilient HTTP client, an HLS playlist parser and writer, and an MPEG-DASH
manifest reader and writer.
Each package stands on its own — they share no code beyond the small
`internal/safe` validator the two writers use. The `hls` and `dash` parsers are
standard library only; their writers wrap `github.com/Eyevinn/hls-m3u8` and
`github.com/Eyevinn/dash-mpd` (MIT), which the packages therefore import.
`httpx` adds `golang.org/x/time/rate` for its token
buckets, and, for the optional browser TLS fingerprints below,
`golang.org/x/net/http2` and `github.com/refraction-networking/utls`. Every
dependency is pure Go: the whole module builds with `CGO_ENABLED=0` on all six
64-bit architectures the CI tests.

```
go get github.com/go-streamkit/streamkit
```

Requires Go 1.26.4 or newer.

## `streamkit/httpx`

A retrying HTTP client with two-bucket, per-host rate limiting, `Retry-After`
handling, and a retry when a response body is cut mid-stream. Configure it once,
then fetch: `Do` retries on transport errors and on 5xx/429, waiting for the
host's rate limit before every attempt.

```go
client, err := httpx.New(httpx.Config{
    UserAgent: "streamkit-example/1.0",
    Timeout:   30 * time.Second,
    Retries:   3,
    RateLimit: httpx.DefaultRateLimit, // requests per second, per host
})
if err != nil {
    log.Fatal(err)
}

body, resp, err := client.Get(context.Background(), "https://host.example/index.m3u8", nil)
if err != nil {
    log.Fatal(err)
}
fmt.Println(resp.StatusCode, len(body))
```

### Browser TLS fingerprints

A TLS handshake says who is calling before the first header does, and some
servers close the connection on sight of the ClientHello `crypto/tls` sends —
whatever the User-Agent would have said, and before any request is written.
`TLSFingerprint` makes the transport present a named browser's hello instead.

```go
client, err := httpx.New(httpx.Config{
    TLSFingerprint: httpx.FingerprintChrome, // or Firefox, Safari, Edge
})
```

The zero value keeps Go's own hello, so nothing changes for callers that do not
ask; a name that is not one is refused by `New` rather than ignored, and
`httpx.TLSFingerprints()` lists the ones there are. A browser hello offers both
`h2` and `http/1.1` over ALPN, so the transport dispatches on what the server
actually chose and speaks it: sending a browser's hello and then always speaking
HTTP/1.1 would be a second, louder tell. Everything above the handshake — the
retries, the per-host rate limiting, the `Retry-After` handling — is unchanged.

## `streamkit/hls`

An HLS (HTTP Live Streaming) parser covering the subset of RFC 8216 that streams
serve in practice: master playlists with variant streams, media playlists with
segments, `EXT-X-BYTERANGE`, `EXT-X-MAP`, and AES-128 keys. Standard library
only.

```go
pl, err := hls.Parse(base, data) // base *url.URL resolves relative URIs; may be nil
if err != nil {
    log.Fatal(err)
}
if pl.Master {
    if best, ok := pl.Best(); ok {
        fmt.Println("highest-bandwidth variant:", best.URL)
    }
} else {
    fmt.Printf("%d segments, %.1fs total\n", len(pl.Segments), pl.TotalDuration())
}
```

### Writing playlists

`hls.MediaPlaylist` and `hls.MasterPlaylist` write a VOD packaging of CMAF/fMP4
(or WebVTT) segments, wrapping Eyevinn's `hls-m3u8` for the tags:

```go
err := hls.MediaPlaylist{
    InitURI: "video1_init.mp4", IndependentSegments: true,
    Segments: []hls.MediaSegment{{URI: "video1_00001.m4s", Duration: 2}, {URI: "video1_00002.m4s", Duration: 2}},
}.Write(w)
```

`EXT-X-TARGETDURATION` is the longest segment rounded to the nearest integer
(RFC 8216 §4.3.3.1), the version is 7 with an `EXT-X-MAP` (which needs 6) and 3
without, and `MasterPlaylist` writes `EXT-X-MEDIA` renditions (`AUDIO`,
`SUBTITLES`) and `EXT-X-STREAM-INF` variants, refusing a variant that names a
group that is not defined. Every URI must be a relative reference that stays in
the playlist's directory (no absolute path, scheme, backslash, `.` or `..`
segment; the rest is percent-escaped), and a name, language or codec that holds
a quote or a line break is refused, so a track name cannot start a new tag.
Output was compared with ffmpeg's `hls` muxer (`-hls_segment_type fmp4`): the
same set of lines for media playlists, the same attributes for the master;
ffmpeg's golden output is in `testdata/ffmpeg`. The segments themselves come from
go-avkit's `container.Segment`.

## `streamkit/dash`

An MPEG-DASH manifest reader that understands `SegmentTemplate` (with
`$Number$`/`$Time$` placeholders and a `SegmentTimeline`), `SegmentList`, and
`SegmentBase`/`BaseURL` single-file representations. It reports what a manifest
really offers — including when a stream keeps video and audio in separate
representations that a downloader would need to mux. Standard library only.

```go
m, err := dash.Parse(base, data) // base *url.URL resolves relative URLs
if err != nil {
    log.Fatal(err)
}
for _, s := range m.Video() {
    fmt.Printf("%s %dx%d, %d segments, needs muxing: %v\n",
        s.Codecs, s.Width, s.Height, len(s.Segments), m.NeedsMuxing(s))
}
```

### Writing a manifest

`dash.Presentation` writes a static MPD (`isoff-live` profile) of one period:
one adaptation set per kind, flagged `segmentAlignment` and `startWithSAP`, each
representation with a `SegmentTemplate` and a `SegmentTimeline`, wrapping
Eyevinn's `dash-mpd`. `dash.Timeline` turns segment spans into `S` entries with
repeat counts, stating `t` only where there is a gap. Templates are checked
(`$Number$`, `$Time$`, `$RepresentationID$`, `$Bandwidth$`, `$$`; no
traversal) and identifiers are letters, digits, `_` and `-`. A written manifest
reads back through `dash.Parse`. Compared with ffmpeg's `dash` muxer: the same
profile, type, durations, content types, codecs, bandwidth, timescales,
templates and timeline entries; ffmpeg puts `frameRate` on the adaptation set,
this on the representation. ISO/IEC 23009-1 is paywalled and was not opened; the
`dash-mpd` types follow its schema. Live, multi-bitrate ladders and encryption
are not covered.

## License

BSD-3-Clause. See [LICENSE](LICENSE).

# streamkit

Mutualized, site-agnostic transport code for streaming media, in pure Go.

`streamkit` gathers three small, self-contained packages that any streaming
client needs and that carry no knowledge of where their bytes come from: a
resilient HTTP client, an HLS playlist parser, and an MPEG-DASH manifest reader.
Each package stands on its own — they share no internal code. `hls` and `dash`
are standard library only; `httpx` adds `golang.org/x/time/rate` for its token
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

## License

BSD-3-Clause. See [LICENSE](LICENSE).

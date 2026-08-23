# streamkit

Mutualized, site-agnostic transport code for streaming media, in pure Go.

`streamkit` gathers three small, self-contained packages that any streaming
client needs and that carry no knowledge of where their bytes come from: a
resilient HTTP client, an HLS playlist parser, and an MPEG-DASH manifest reader.
Each package stands on its own — they share no internal code and, apart from
`golang.org/x/time/rate` in `httpx`, depend only on the standard library.

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

// Package provider resolves a provider page/room URL (e.g. a Bilibili video page
// or a Douyu room) into the direct CDN stream URL ffmpeg should pull, plus the
// HTTP headers the CDN requires and whether the stream is live.
//
// Resolvers are pure-Go; no external runtime (Python/yt-dlp/biliup) is needed.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Result is a resolved source.
type Result struct {
	// URL is the direct media URL ffmpeg consumes.
	URL string
	// Headers are HTTP request headers the CDN requires (User-Agent, Referer).
	Headers map[string]string
	// Live is true for never-ending streams (restart on exit rather than stop).
	Live bool
}

// Resolver turns a provider page/room URL into a Result.
type Resolver interface {
	Resolve(ctx context.Context, pageURL string) (*Result, error)
}

// httpClient is shared by the concrete resolvers. It is bounded so a hung
// provider site can't stall a stream start indefinitely.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// userAgent is a realistic browser UA; provider CDNs reject requests without one.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// registry maps a provider name (as stored on a Stream) to its Resolver.
var registry = map[string]Resolver{
	"bilibili": &bilibiliResolver{},
	"douyu":    &douyuResolver{},
}

// Get returns the Resolver for name, or (nil, false) if name is unknown.
func Get(name string) (Resolver, bool) {
	r, ok := registry[name]
	return r, ok
}

// headers returns the common request headers for a given Referer origin.
func headers(referer string) map[string]string {
	return map[string]string{
		"User-Agent": userAgent,
		"Referer":    referer,
	}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// fetchText GETs url with the given headers and returns the body as a string.
func fetchText(ctx context.Context, url string, hdrs map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeBody decodes a JSON response body. The caller owns closing the body.
func decodeBody(resp *http.Response, out any) error {
	return json.NewDecoder(resp.Body).Decode(out)
}

// getJSON GETs url with the given headers and decodes the JSON body into out.
func getJSON(ctx context.Context, url string, hdrs map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

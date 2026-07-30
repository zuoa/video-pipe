package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// downloadHTTPClient has no whole-request timeout because a VOD download can
// legitimately take a long time. Cancellation is controlled by ctx.
var downloadHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &http.Client{Transport: transport}
}()

// DownloadToFile downloads a resolved provider URL to dst. Data is first
// written beside dst and atomically renamed only after a complete, non-empty
// response, so ffmpeg can never observe a partial cache file.
func DownloadToFile(ctx context.Context, rawURL string, headers map[string]string, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := *downloadHTTPClient
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		// Keep the provider's required Referer/User-Agent if the CDN redirects
		// to a different host.
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("provider media returned http %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}

	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(tmp)
		}
	}()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	if n == 0 {
		return 0, fmt.Errorf("downloaded file is empty")
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("incomplete download: got %d bytes, want %d", n, resp.ContentLength)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return n, err
	}
	complete = true
	return n, nil
}

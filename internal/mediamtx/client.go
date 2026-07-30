// Package mediamtx is a thin client for the MediaMTX Control API (v3).
package mediamtx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the MediaMTX Control API.
type Client struct {
	base      string
	auth      string // Basic auth header value, empty if none
	hc        *http.Client
}

// New creates a client. user/pass enable HTTP Basic Auth, required when the
// backend runs in a separate container (the `api` action is localhost-only by
// default).
func New(base, user, pass string) *Client {
	c := &Client{base: base, hc: &http.Client{Timeout: 5 * time.Second}}
	if user != "" {
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		c.auth = "Basic " + token
	}
	return c
}

// PathState is the runtime state of a MediaMTX path that we care about.
type PathState struct {
	Online       bool  // a live source is publishing real data — the authoritative "active" signal
	Available    bool  // online OR the offline/loop segment is playing
	Readers      int   // current consumer count
	InboundBytes int64 // bytes received from the source
}

type pathResp struct {
	Online       bool `json:"online"`
	Available    bool `json:"available"`
	Readers      []struct {
		Type string `json:"type"`
	} `json:"readers"`
	InboundBytes int64 `json:"inboundBytes"`
}

// ErrNotFound means the path has no connected publisher yet (stream warming up
// or stopped). Callers should treat this as "not online" rather than an error.
var ErrNotFound = errors.New("path not found")

// GetPath returns the runtime state of a path. A 404 yields (nil, nil): the path
// exists but nothing is publishing to it. A network/HTTP error yields (nil, err);
// callers should degrade gracefully and NOT act on it (the ffmpeg watchdog owns
// process health).
func (c *Client) GetPath(ctx context.Context, name string) (*PathState, error) {
	u := fmt.Sprintf("%s/v3/paths/get/%s", c.base, url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("mediamtx GET %s: %s (%s)", u, resp.Status, strings.TrimSpace(string(body)))
	}

	var pr pathResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode path response: %w", err)
	}
	return &PathState{
		Online:       pr.Online,
		Available:    pr.Available,
		Readers:      len(pr.Readers),
		InboundBytes: pr.InboundBytes,
	}, nil
}

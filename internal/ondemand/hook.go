// Package ondemand contains the tiny foreground helper launched by MediaMTX's
// runOnDemand hook. It asks the long-running backend to own ffmpeg, then stays
// alive until MediaMTX signals that the path has no readers.
package ondemand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBackendURL = "http://video-pipe:8081"
	retryInterval     = 2 * time.Second
	heartbeatInterval = 10 * time.Second
)

type request struct {
	Name  string `json:"name"`
	Lease string `json:"lease"`
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("backend returned HTTP %d: %s", e.code, e.body)
}

// Run acquires and maintains one demand lease until ctx is canceled. MTX_PATH
// is supplied by MediaMTX. Heartbeats are idempotent and recreate the ffmpeg
// process if the backend container restarts while a reader remains connected.
func Run(ctx context.Context, log *slog.Logger) error {
	name := strings.TrimSpace(os.Getenv("MTX_PATH"))
	if name == "" {
		return fmt.Errorf("MTX_PATH is empty")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("VIDEO_PIPE_ON_DEMAND_URL")), "/")
	if baseURL == "" {
		baseURL = defaultBackendURL
	}
	hostname, _ := os.Hostname()
	lease := fmt.Sprintf("%s-%d-%d", hostname, os.Getpid(), time.Now().UnixNano())
	payload := request{Name: name, Lease: lease}
	client := &http.Client{Timeout: 5 * time.Second}

	// Always issue a release on exit. This also covers the ambiguous case where
	// the backend accepted an acquire but the HTTP response was lost while
	// MediaMTX was terminating the command; releasing an unknown lease is a no-op.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := post(releaseCtx, client, baseURL+"/release", payload); err != nil {
			log.Warn("on-demand release failed", "stream", name, "err", err)
		}
	}()

	for {
		err := post(ctx, client, baseURL+"/acquire", payload)
		if err == nil {
			log.Info("on-demand lease acquired", "stream", name)
			break
		}
		if terminal(err) {
			return err
		}
		log.Debug("on-demand stream not ready; retrying", "stream", name, "err", err)
		if !sleep(ctx, retryInterval) {
			return nil
		}
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := post(ctx, client, baseURL+"/acquire", payload); err != nil {
				if terminal(err) {
					return err
				}
				log.Warn("on-demand heartbeat failed", "stream", name, "err", err)
			}
		}
	}
}

func post(ctx context.Context, client *http.Client, target string, payload request) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(responseBody))}
	}
	return nil
}

func terminal(err error) bool {
	se, ok := err.(*statusError)
	return ok && (se.code == http.StatusBadRequest || se.code == http.StatusNotFound || se.code == http.StatusConflict)
}

func sleep(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

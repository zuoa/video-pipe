package ondemand

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunAcquiresAndReleasesLease(t *testing.T) {
	acquired := make(chan request, 1)
	released := make(chan request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/acquire":
			w.WriteHeader(http.StatusNoContent)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			acquired <- req
			return
		case "/release":
			released <- req
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Setenv("MTX_PATH", "camera_01")
	t.Setenv("VIDEO_PIPE_ON_DEMAND_URL", upstream.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	var first request
	select {
	case first = <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not acquire a lease")
	}
	if first.Name != "camera_01" || first.Lease == "" {
		t.Fatalf("acquire request = %#v", first)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	select {
	case last := <-released:
		if last != first {
			t.Fatalf("release request = %#v, want %#v", last, first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not release its lease")
	}
}

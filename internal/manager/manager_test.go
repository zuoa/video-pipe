package manager

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"video-pipe/internal/mediamtx"
	"video-pipe/internal/model"
	"video-pipe/internal/provider"
	"video-pipe/internal/store"
)

func TestStartKeepsEnabledStreamsIdle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "streams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.Create(context.Background(), model.Stream{
		Name:         "idle_test",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateRunning,
	})
	if err != nil {
		t.Fatal(err)
	}

	m := New(st, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	got := len(m.handles)
	m.mu.Unlock()
	if got != 0 {
		t.Fatalf("startup handles = %d, want 0", got)
	}
	cancel()
	m.Wait()
}

func TestExpireDemandLeasesStopsOrphanedEntry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	m := &Manager{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		handles: map[string]*entry{"cam": {stream: model.Stream{Name: "cam"}, cancel: cancel, done: done}},
		demands: map[string]map[string]time.Time{"cam": {"lease": time.Now().Add(-demandLeaseTTL)}},
	}
	m.expireDemandLeases(time.Now())
	if len(m.handles) != 0 || len(m.demands) != 0 {
		t.Fatalf("expired lease was not cleaned up: handles=%d demands=%d", len(m.handles), len(m.demands))
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expired lease did not cancel its process context")
	}
}

func TestCacheProviderVOD_DownloadsOnceAndReusesCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("video contents"))
	}))
	defer server.Close()

	m := &Manager{
		cacheDir: t.TempDir(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	stream := model.Stream{
		Name:      "bili_1",
		SourceURL: "https://www.bilibili.com/video/BV1xx",
		Provider:  "bilibili",
	}
	resolved := &provider.Result{URL: server.URL}

	first, err := m.cacheProviderVOD(context.Background(), stream, resolved)
	if err != nil {
		t.Fatalf("first cacheProviderVOD: %v", err)
	}
	second, err := m.cacheProviderVOD(context.Background(), stream, resolved)
	if err != nil {
		t.Fatalf("second cacheProviderVOD: %v", err)
	}
	if first != second {
		t.Fatalf("cache paths differ: %q != %q", first, second)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("download requests = %d, want 1", got)
	}
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) != "video contents" {
		t.Fatalf("cache contents = %q", body)
	}
}

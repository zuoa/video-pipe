package manager

import (
	"context"
	"errors"
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

func TestPickRandom(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "streams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	mustCreate := func(name, state string) {
		t.Helper()
		if _, err := st.Create(ctx, model.Stream{
			Name:         name,
			SourceType:   model.SourceTest,
			Live:         true,
			DesiredState: state,
		}); err != nil {
			t.Fatal(err)
		}
	}

	m := New(st, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := m.PickRandom(ctx); !errors.Is(err, ErrNoEnabled) {
		t.Fatalf("empty store: err = %v, want ErrNoEnabled", err)
	}

	mustCreate("stopped_1", model.StateStopped)
	if _, err := m.PickRandom(ctx); !errors.Is(err, ErrNoEnabled) {
		t.Fatalf("all stopped: err = %v, want ErrNoEnabled", err)
	}

	mustCreate("live_a", model.StateRunning)
	mustCreate("live_b", model.StateRunning)
	mustCreate(model.ReservedPathRandom, model.StateRunning)
	got, err := m.PickRandom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "live_a" && got.Name != "live_b" {
		t.Fatalf("picked %q, want one of live_a/live_b", got.Name)
	}

	all, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	enabled := enabledForRandom(all)
	if len(enabled) != 2 {
		t.Fatalf("enabled pool = %d, want 2 (stopped and reserved path excluded)", len(enabled))
	}
	m.randN = func(n int) int {
		if n != 2 {
			t.Fatalf("randN(%d), want 2 enabled streams", n)
		}
		return 1
	}
	got, err = m.PickRandom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != enabled[1].Name {
		t.Fatalf("forced pick = %q, want %q", got.Name, enabled[1].Name)
	}
}

func TestAcquireDemandRandomUsesBackingSource(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "streams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := st.Create(ctx, model.Stream{
		Name:         "cam_a",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, model.Stream{
		Name:         "stopped_cam",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateStopped,
	}); err != nil {
		t.Fatal(err)
	}

	m := New(st, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.stopAndClearDemand(model.ReservedPathRandom)
		cancel()
		m.Wait()
	})

	if err := m.AcquireDemand(model.ReservedPathRandom, "lease-1"); err != nil {
		t.Fatalf("AcquireDemand(random): %v", err)
	}
	m.mu.Lock()
	e, ok := m.handles[model.ReservedPathRandom]
	_, backingHandle := m.handles["cam_a"]
	m.mu.Unlock()
	if !ok {
		t.Fatal("expected handle for path random")
	}
	if e.stream.Name != model.ReservedPathRandom {
		t.Fatalf("publish name = %q, want random", e.stream.Name)
	}
	if e.stream.SourceType != model.SourceTest {
		t.Fatalf("source type = %q, want test", e.stream.SourceType)
	}
	if backingHandle {
		t.Fatal("backing stream should stay idle until its own path is requested")
	}

	if err := m.AcquireDemand(model.ReservedPathRandom, "lease-1"); err != nil {
		t.Fatalf("heartbeat AcquireDemand: %v", err)
	}
}

func TestAcquireDemandRandomEmptyPool(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "streams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := st.Create(ctx, model.Stream{
		Name:         "only_stopped",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateStopped,
	}); err != nil {
		t.Fatal(err)
	}
	m := New(st, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Wait()

	if err := m.AcquireDemand(model.ReservedPathRandom, "lease-1"); !errors.Is(err, ErrNoEnabled) {
		t.Fatalf("err = %v, want ErrNoEnabled", err)
	}
	m.mu.Lock()
	n := len(m.handles)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("handles = %d, want 0", n)
	}
}

func TestNewEntryAsKeepsProviderCachePath(t *testing.T) {
	m := New(nil, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.ctx = context.Background()
	s := model.Stream{
		Name:       "bili_1",
		SourceURL:  "https://www.bilibili.com/video/BV1xx411c7mD",
		SourceType: model.SourceHTTP,
		Provider:   "bilibili",
	}
	want := m.providerCachePath(s)
	alias := s
	alias.Name = model.ReservedPathRandom
	if m.providerCachePath(alias) == want {
		t.Fatal("cache path must include the original stream name")
	}

	resolve, err := m.resolverFor(s)
	if err != nil {
		t.Fatal(err)
	}
	if resolve == nil {
		t.Fatal("expected a VOD cache resolver")
	}
	got, _, live, err := resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live {
		t.Fatal("cached VOD must not be treated as live")
	}
	if got != want {
		t.Fatalf("resolver path = %q, want %q", got, want)
	}

	e, err := m.newEntryAs(model.ReservedPathRandom, s)
	if err != nil {
		t.Fatal(err)
	}
	if e.stream.Name != model.ReservedPathRandom {
		t.Fatalf("entry name = %q, want random", e.stream.Name)
	}
	e.cancel()
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

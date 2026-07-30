package manager

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"video-pipe/internal/model"
	"video-pipe/internal/provider"
)

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

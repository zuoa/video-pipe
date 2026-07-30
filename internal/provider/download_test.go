package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadToFile(t *testing.T) {
	const body = "complete video contents"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); got != "https://www.bilibili.com" {
			t.Errorf("Referer = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	dst := filepath.Join(t.TempDir(), "nested", "video.media")
	n, err := DownloadToFile(context.Background(), server.URL, map[string]string{
		"Referer":    "https://www.bilibili.com",
		"User-Agent": "test-agent",
	}, dst)
	if err != nil {
		t.Fatalf("DownloadToFile: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("downloaded bytes = %d, want %d", n, len(body))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != body {
		t.Fatalf("downloaded body = %q", got)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("partial file remains after successful download: %v", err)
	}
}

func TestDownloadToFile_RemovesPartialOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()

	dst := filepath.Join(t.TempDir(), "video.media")
	if _, err := DownloadToFile(context.Background(), server.URL, nil, dst); err == nil {
		t.Fatal("DownloadToFile unexpectedly succeeded")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination exists after failed download: %v", err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("partial file remains after failed download: %v", err)
	}
}

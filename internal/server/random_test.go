package server

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"video-pipe/internal/config"
	"video-pipe/internal/manager"
	"video-pipe/internal/mediamtx"
	"video-pipe/internal/model"
	"video-pipe/internal/store"
)

func newTestAPI(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(st, mediamtx.New("http://127.0.0.1:1", "", ""), "mediamtx", t.TempDir(), log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Wait)
	s := &Server{
		cfg: config.Config{
			PlaybackHost: "localhost",
			Enabled: map[string]bool{
				"rtsp": true, "rtmp": true, "hls": true, "webrtc": true, "srt": true,
			},
		},
		store:       st,
		mgr:         mgr,
		log:         log,
		hlsProxy:    http.NotFoundHandler(),
		webrtcProxy: http.NotFoundHandler(),
		static:      http.NotFoundHandler(),
	}
	s.h = s.routes()
	return s, st
}

func TestRandomStreamAPI(t *testing.T) {
	s, st := newTestAPI(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/streams/random")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("empty pool status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := st.Create(context.Background(), model.Stream{
		Name:         "cam_a",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(context.Background(), model.Stream{
		Name:         "idle_cam",
		SourceType:   model.SourceTest,
		Live:         true,
		DesiredState: model.StateStopped,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err = http.Get(ts.URL + "/api/streams/random")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out streamOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "cam_a" {
		t.Fatalf("picked %q, want cam_a", out.Name)
	}
	if out.URLs["rtsp"] != "rtsp://localhost:8554/cam_a" {
		t.Fatalf("rtsp url = %q", out.URLs["rtsp"])
	}
}

func TestCreateRejectsReservedRandomName(t *testing.T) {
	s, _ := newTestAPI(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/streams", "application/json", strings.NewReader(`{"name":"random","source_type":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/api/streams/random/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("status path status = %d, want 400", resp2.StatusCode)
	}
}

func TestIndexBootstrapsRandomURLs(t *testing.T) {
	s, _ := newTestAPI(t)
	tmpl, err := template.ParseFS(assets, "templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s.tmpl = tmpl
	rec := httptest.NewRecorder()
	s.index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`/playback/hls/random/index.m3u8`,
		`rtsp://localhost:8554/random`,
		`window.BOOT`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index HTML missing %q", want)
		}
	}
}

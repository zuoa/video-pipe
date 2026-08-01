package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlaybackURLsUseSameOriginForBrowserProtocols(t *testing.T) {
	urls := playbackURLs("video.example.com", "camera_01", map[string]bool{
		"rtsp": true, "rtmp": true, "hls": true, "webrtc": true, "srt": true,
	})

	if got, want := urls["hls"], "/playback/hls/camera_01/index.m3u8"; got != want {
		t.Fatalf("HLS URL = %q, want %q", got, want)
	}
	if got, want := urls["webrtc"], "/playback/webrtc/camera_01"; got != want {
		t.Fatalf("WebRTC URL = %q, want %q", got, want)
	}
	if got, want := urls["rtsp"], "rtsp://video.example.com:8554/camera_01"; got != want {
		t.Fatalf("RTSP URL = %q, want %q", got, want)
	}
}

func TestPlaybackProxyStripsPrefixAndRewritesLocation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/camera_01/whep"; got != want {
			t.Errorf("upstream path = %q, want %q", got, want)
		}
		w.Header().Set("Location", "/camera_01/whep/session-id")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "answer")
	}))
	defer upstream.Close()

	proxy, err := playbackProxy(upstream.URL, "/playback/webrtc")
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewServer(http.StripPrefix("/playback/webrtc", proxy))
	defer public.Close()

	resp, err := http.Post(public.URL+"/playback/webrtc/camera_01/whep", "application/sdp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := resp.Header.Get("Location"), "/playback/webrtc/camera_01/whep/session-id"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestPlaybackRoutesDoNotConflictWithIndex(t *testing.T) {
	s := &Server{
		hlsProxy:    http.NotFoundHandler(),
		webrtcProxy: http.NotFoundHandler(),
		static:      http.NotFoundHandler(),
	}
	_ = s.routes()
}

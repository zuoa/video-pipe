// Package server exposes the management HTTP API and renders the Web UI.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"video-pipe/internal/config"
	"video-pipe/internal/manager"
	"video-pipe/internal/store"
)

// Server is the HTTP application: API + rendered UI, wired to the store and manager.
type Server struct {
	cfg         config.Config
	store       *store.Store
	mgr         *manager.Manager
	log         *slog.Logger
	tmpl        *template.Template
	static      http.Handler
	hlsProxy    http.Handler
	webrtcProxy http.Handler
	h           http.Handler
}

// New parses embedded templates, prepares the static file server, and registers routes.
func New(cfg config.Config, st *store.Store, mgr *manager.Manager, log *slog.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(assets, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	staticSub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("static sub fs: %w", err)
	}
	hlsProxy, err := playbackProxy(cfg.MediaMTXHLS, "/playback/hls")
	if err != nil {
		return nil, fmt.Errorf("HLS proxy: %w", err)
	}
	webrtcProxy, err := playbackProxy(cfg.MediaMTXWebRTC, "/playback/webrtc")
	if err != nil {
		return nil, fmt.Errorf("WebRTC proxy: %w", err)
	}

	s := &Server{
		cfg:         cfg,
		store:       st,
		mgr:         mgr,
		log:         log,
		tmpl:        tmpl,
		static:      http.FileServer(http.FS(staticSub)),
		hlsProxy:    hlsProxy,
		webrtcProxy: webrtcProxy,
	}
	s.h = s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.h.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/streams", s.listStreams)
	mux.HandleFunc("GET /api/streams/random", s.randomStream)
	mux.HandleFunc("POST /api/streams", s.createStream)
	mux.HandleFunc("POST /api/uploads", s.uploadFile)
	mux.HandleFunc("GET /api/streams/{name}/urls", s.streamURLs)
	mux.HandleFunc("GET /api/streams/{name}/status", s.streamStatus)
	mux.HandleFunc("POST /api/streams/{name}/start", s.startStream)
	mux.HandleFunc("POST /api/streams/{name}/stop", s.stopStream)
	mux.HandleFunc("DELETE /api/streams/{name}", s.deleteStream)
	// Browser playback is kept on the application's origin. This avoids CORS
	// and HTTPS mixed-content failures and means ports 8888/8889 do not have to
	// be exposed publicly. WebRTC media still uses the ICE port (8189).
	hlsPlayback := http.StripPrefix("/playback/hls", s.hlsProxy)
	mux.Handle("GET /playback/hls/", hlsPlayback)
	mux.Handle("HEAD /playback/hls/", hlsPlayback)
	webrtcPlayback := http.StripPrefix("/playback/webrtc", s.webrtcProxy)
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "POST", "PATCH", "DELETE"} {
		mux.Handle(method+" /playback/webrtc/", webrtcPlayback)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	return mux
}

// playbackProxy forwards an HTTP playback endpoint to MediaMTX and rewrites
// absolute-path redirects so clients remain under the public proxy prefix.
func playbackProxy(rawTarget, publicPrefix string) (http.Handler, error) {
	target, err := url.Parse(rawTarget)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL %q", rawTarget)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if location := resp.Header.Get("Location"); strings.HasPrefix(location, "/") {
			resp.Header.Set("Location", publicPrefix+location)
		}
		return nil
	}
	return proxy, nil
}

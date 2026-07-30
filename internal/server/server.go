// Package server exposes the management HTTP API and renders the Web UI.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"video-pipe/internal/config"
	"video-pipe/internal/manager"
	"video-pipe/internal/store"
)

// Server is the HTTP application: API + rendered UI, wired to the store and manager.
type Server struct {
	cfg    config.Config
	store  *store.Store
	mgr    *manager.Manager
	log    *slog.Logger
	tmpl   *template.Template
	static http.Handler
	h      http.Handler
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

	s := &Server{
		cfg:    cfg,
		store:  st,
		mgr:    mgr,
		log:    log,
		tmpl:   tmpl,
		static: http.FileServer(http.FS(staticSub)),
	}
	s.h = s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.h.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/streams", s.listStreams)
	mux.HandleFunc("POST /api/streams", s.createStream)
	mux.HandleFunc("POST /api/uploads", s.uploadFile)
	mux.HandleFunc("GET /api/streams/{name}/urls", s.streamURLs)
	mux.HandleFunc("GET /api/streams/{name}/status", s.streamStatus)
	mux.HandleFunc("POST /api/streams/{name}/start", s.startStream)
	mux.HandleFunc("POST /api/streams/{name}/stop", s.stopStream)
	mux.HandleFunc("DELETE /api/streams/{name}", s.deleteStream)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	return mux
}

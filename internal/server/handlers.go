package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"video-pipe/internal/ffmpeg"
	"video-pipe/internal/model"
	"video-pipe/internal/store"
)

// streamOut is the JSON representation of a stream plus its live status and URLs.
type streamOut struct {
	model.Stream
	State         string            `json:"state"`
	MtxOnline     bool              `json:"mtx_online"`
	Readers       int               `json:"readers"`
	RestartCount  int               `json:"restart_count"`
	LastError     string            `json:"last_error"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
	URLs          map[string]string `json:"urls"`
}

func (s *Server) toStreamOut(st model.Stream) streamOut {
	out := streamOut{
		Stream: st,
		URLs:   playbackURLs(s.cfg.PlaybackHost, st.Name),
	}
	if snap, ok := s.mgr.HandleSnapshot(st.Name); ok {
		out.State = string(snap.State)
		out.MtxOnline = snap.MtxOnline
		out.Readers = snap.Readers
		out.RestartCount = snap.RestartCount
		out.LastError = snap.LastError
		out.LastHeartbeat = snap.LastHeartbeat
	} else {
		out.State = string(ffmpeg.StateStopped)
	}
	return out
}

// playbackURLs builds the per-protocol playback URLs for a path using the
// externally reachable host (PLAYBACK_HOST). MediaMTX default ports are used.
func playbackURLs(host, name string) map[string]string {
	return map[string]string{
		"rtsp":   fmt.Sprintf("rtsp://%s:8554/%s", host, name),
		"rtmp":   fmt.Sprintf("rtmp://%s:1935/%s", host, name),
		"hls":    fmt.Sprintf("http://%s:8888/%s/index.m3u8", host, name),
		"webrtc": fmt.Sprintf("http://%s:8889/%s", host, name),
		"srt":    fmt.Sprintf("srt://%s:8890?streamid=#!::m=request,r=%s", host, name),
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", map[string]any{
		"SourceTypes":  model.ValidSourceTypes,
		"PlaybackHost": s.cfg.PlaybackHost,
	}); err != nil {
		s.log.Error("render index", "err", err)
	}
}

func (s *Server) listStreams(w http.ResponseWriter, r *http.Request) {
	streams, err := s.store.List(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]streamOut, 0, len(streams))
	for _, st := range streams {
		out = append(out, s.toStreamOut(st))
	}
	writeJSON(w, http.StatusOK, out)
}

type createReq struct {
	Name       string `json:"name"`
	SourceURL  string `json:"source_url"`
	SourceType string `json:"source_type"`
}

func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, "invalid JSON body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if !model.ValidName(name) {
		badRequest(w, "invalid name: use lowercase letters, digits, '-' or '_' (max 64)")
		return
	}
	srcType := req.SourceType
	if srcType == "" {
		srcType = model.SourceAuto
	}
	if !model.IsValidSourceType(srcType) {
		badRequest(w, "invalid source_type")
		return
	}
	srcType = model.DeriveSourceType(srcType, req.SourceURL)
	if srcType != model.SourceTest && strings.TrimSpace(req.SourceURL) == "" {
		badRequest(w, "source_url is required for this source type")
		return
	}

	st := model.Stream{
		Name:         name,
		SourceURL:    strings.TrimSpace(req.SourceURL),
		SourceType:   srcType,
		Live:         model.IsLive(srcType),
		DesiredState: model.StateRunning,
	}
	created, err := s.store.Create(r.Context(), st)
	if err != nil {
		serverError(w, err) // includes UNIQUE constraint -> maps to 409 below
		return
	}
	if err := s.mgr.EnsureRunning(created); err != nil {
		s.log.Error("start stream after create", "name", created.Name, "err", err)
	}

	out := s.toStreamOut(created)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) streamURLs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.store.Get(r.Context(), name); err != nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, playbackURLs(s.cfg.PlaybackHost, name))
}

func (s *Server) streamStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, err := s.store.Get(r.Context(), name)
	if err != nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) startStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, err := s.store.Get(r.Context(), name)
	if err != nil {
		notFound(w)
		return
	}
	if err := s.mgr.EnsureRunning(st); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) stopStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.store.Get(r.Context(), name); err != nil {
		notFound(w)
		return
	}
	if err := s.mgr.EnsureStopped(name); err != nil {
		serverError(w, err)
		return
	}
	st, _ := s.store.Get(r.Context(), name)
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) deleteStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.mgr.Delete(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return
		}
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	defer r.Body.Close()
	return dec.Decode(v)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "stream not found"})
}

func serverError(w http.ResponseWriter, err error) {
	// A UNIQUE-constraint violation on name is a conflict, not a server fault.
	if strings.Contains(err.Error(), "UNIQUE") {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a stream with this name already exists"})
		return
	}
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

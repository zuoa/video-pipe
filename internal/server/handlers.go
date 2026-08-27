package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"video-pipe/internal/ffmpeg"
	"video-pipe/internal/manager"
	"video-pipe/internal/model"
	"video-pipe/internal/provider"
	"video-pipe/internal/store"
)

// streamOut is the JSON representation of a stream plus its live status and URLs.
type streamOut struct {
	model.Stream
	State         string            `json:"state"`
	MtxOnline     bool              `json:"mtx_online"`
	Readers       int               `json:"readers"`
	ReaderList    []model.Reader    `json:"reader_list"`
	RestartCount  int               `json:"restart_count"`
	LastError     string            `json:"last_error"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
	URLs          map[string]string `json:"urls"`
}

func (s *Server) toStreamOut(st model.Stream) streamOut {
	out := streamOut{
		Stream: st,
		URLs:   playbackURLs(s.cfg.PlaybackHost, st.Name, s.cfg.Enabled),
	}
	if snap, ok := s.mgr.HandleSnapshot(st.Name); ok {
		out.State = string(snap.State)
		out.MtxOnline = snap.MtxOnline
		out.Readers = snap.Readers
		out.ReaderList = snap.ReaderList
		out.RestartCount = snap.RestartCount
		out.LastError = snap.LastError
		out.LastHeartbeat = snap.LastHeartbeat
	} else if prep, ok := s.mgr.PreparationSnapshot(st.Name); ok && prep.State == "preparing" {
		out.State = string(ffmpeg.StatePreparing)
	} else if ok && prep.State == "error" {
		out.State = string(ffmpeg.StateError)
		out.LastError = prep.LastError
	} else if st.DesiredState == model.StateRunning {
		out.State = string(ffmpeg.StateIdle)
	} else {
		out.State = string(ffmpeg.StateStopped)
	}
	return out
}

// playbackURLs builds per-protocol playback URLs, limited to enabled protocols.
// Browser protocols use same-origin proxy paths so they also work behind HTTPS
// and don't require exposing the MediaMTX HTTP ports. Native-player protocols
// still use the externally reachable PLAYBACK_HOST.
func playbackURLs(host, name string, enabled map[string]bool) map[string]string {
	all := map[string]string{
		"rtsp":   fmt.Sprintf("rtsp://%s:8554/%s", host, name),
		"rtmp":   fmt.Sprintf("rtmp://%s:1935/%s", host, name),
		"hls":    fmt.Sprintf("/playback/hls/%s/index.m3u8", name),
		"webrtc": fmt.Sprintf("/playback/webrtc/%s", name),
		"srt":    fmt.Sprintf("srt://%s:8890?streamid=#!::m=request,r=%s", host, name),
	}
	out := make(map[string]string, len(all))
	for k, v := range all {
		if enabled[k] {
			out[k] = v
		}
	}
	return out
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	boot, err := json.Marshal(map[string]any{
		"random_urls": playbackURLs(s.cfg.PlaybackHost, model.ReservedPathRandom, s.cfg.Enabled),
	})
	if err != nil {
		s.log.Error("marshal boot config", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", map[string]any{
		"PlaybackHost": s.cfg.PlaybackHost,
		"BootJSON":     template.JS(boot),
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
	Provider   string `json:"provider"` // optional resolver: "", "bilibili", "douyu"
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
	if model.IsReservedPath(name) {
		badRequest(w, "random is a reserved playback path")
		return
	}
	prov := strings.TrimSpace(req.Provider)
	if prov != "" && !model.IsValidProvider(prov) {
		badRequest(w, "invalid provider")
		return
	}
	if prov == "" {
		// Auto-detect provider page/room URLs (bilibili.com, douyu.com). Feeding
		// such a page URL to ffmpeg directly can never work — it returns HTML —
		// so upgrading to the resolver is strictly better than failing later.
		prov = provider.DetectFromURL(req.SourceURL)
	}

	srcType := req.SourceType
	if prov != "" {
		// Provider source: the page/room URL is resolved to an http stream at runtime.
		srcType = model.SourceHTTP
	} else {
		if srcType == "" {
			srcType = model.SourceAuto
		}
		if !model.IsValidSourceType(srcType) {
			badRequest(w, "invalid source_type")
			return
		}
		srcType = model.DeriveSourceType(srcType, req.SourceURL)
	}
	if srcType != model.SourceTest && strings.TrimSpace(req.SourceURL) == "" {
		badRequest(w, "source_url is required for this source type")
		return
	}

	st := model.Stream{
		Name:         name,
		SourceURL:    strings.TrimSpace(req.SourceURL),
		SourceType:   srcType,
		Provider:     prov,
		Live:         model.IsLive(srcType),
		DesiredState: model.StateRunning,
	}
	created, err := s.store.Create(r.Context(), st)
	if err != nil {
		serverError(w, err) // includes UNIQUE constraint -> maps to 409 below
		return
	}
	// Creating an enabled stream no longer launches ffmpeg. Bilibili VODs are
	// downloaded in the background; every source starts publishing only when a
	// MediaMTX reader requests its path.
	s.mgr.Prepare(created)

	out := s.toStreamOut(created)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) randomStream(w http.ResponseWriter, r *http.Request) {
	st, err := s.mgr.PickRandom(r.Context())
	if err != nil {
		if errors.Is(err, manager.ErrNoEnabled) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no enabled streams"})
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) streamURLs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if rejectReservedPath(w, name) {
		return
	}
	if _, err := s.store.Get(r.Context(), name); err != nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, playbackURLs(s.cfg.PlaybackHost, name, s.cfg.Enabled))
}

func (s *Server) streamStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if rejectReservedPath(w, name) {
		return
	}
	st, err := s.store.Get(r.Context(), name)
	if err != nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) startStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if rejectReservedPath(w, name) {
		return
	}
	st, err := s.store.Get(r.Context(), name)
	if err != nil {
		notFound(w)
		return
	}
	if err := s.mgr.Enable(st); err != nil {
		serverError(w, err)
		return
	}
	st.DesiredState = model.StateRunning
	writeJSON(w, http.StatusOK, s.toStreamOut(st))
}

func (s *Server) stopStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if rejectReservedPath(w, name) {
		return
	}
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
	if rejectReservedPath(w, name) {
		return
	}
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

type uploadResp struct {
	Path string `json:"path"` // container path ffmpeg reads (becomes source_url)
	Name string `json:"name"` // original client-side filename
	Size int64  `json:"size"`
}

// uploadFile receives a single multipart upload ("file" field) and streams it to
// the upload directory. Returns the container path, which the UI feeds back as
// source_url when creating a file-type stream.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		badRequest(w, "expected multipart/form-data with a 'file' part")
		return
	}
	part, err := nextFilePart(mr)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	defer part.Close()

	orig := strings.TrimSpace(part.FileName())
	if orig == "" {
		badRequest(w, "file part has no filename")
		return
	}
	// Strip any path components and prefix a timestamp so concurrent uploads of
	// the same name don't collide or overwrite, and traversal is impossible.
	name := fmt.Sprintf("%d-%s", time.Now().UnixMilli(), filepath.Base(orig))
	dst := filepath.Join(s.cfg.UploadDir, name)

	f, err := os.Create(dst)
	if err != nil {
		serverError(w, err)
		return
	}

	limit := s.cfg.UploadMaxBytes
	if limit > 0 {
		// Cap one byte past the limit so we can detect an over-sized upload.
		n, copyErr := io.Copy(f, io.LimitReader(part, limit+1))
		if cerr := f.Close(); cerr != nil {
			os.Remove(dst)
			serverError(w, cerr)
			return
		}
		if copyErr != nil {
			os.Remove(dst)
			serverError(w, copyErr)
			return
		}
		if n > limit {
			os.Remove(dst)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload exceeds UPLOAD_MAX_BYTES"})
			return
		}
		writeJSON(w, http.StatusCreated, uploadResp{Path: dst, Name: orig, Size: n})
		return
	}

	n, copyErr := io.Copy(f, part)
	if cerr := f.Close(); cerr != nil {
		os.Remove(dst)
		serverError(w, cerr)
		return
	}
	if copyErr != nil {
		os.Remove(dst)
		serverError(w, copyErr)
		return
	}
	writeJSON(w, http.StatusCreated, uploadResp{Path: dst, Name: orig, Size: n})
}

// nextFilePart walks a multipart reader until it finds the "file" form field.
func nextFilePart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return nil, errors.New("no 'file' part in upload")
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart: %w", err)
		}
		if p.FormName() == "file" {
			return p, nil
		}
	}
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

func rejectReservedPath(w http.ResponseWriter, name string) bool {
	if !model.IsReservedPath(name) {
		return false
	}
	badRequest(w, "random is a reserved playback path")
	return true
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

package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"video-pipe/internal/manager"
	"video-pipe/internal/model"
	"video-pipe/internal/store"
)

type demandRequest struct {
	Name  string `json:"name"`
	Lease string `json:"lease"`
}

// NewOnDemandHandler serves the container-internal callback used by the
// MediaMTX runOnDemand helper. It is intentionally mounted on a separate,
// unpublished listener instead of the public management server.
func NewOnDemandHandler(mgr *manager.Manager, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /acquire", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeDemandRequest(w, r)
		if !ok {
			return
		}
		err := mgr.AcquireDemand(req.Name, req.Lease)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "stream not found", http.StatusNotFound)
		case errors.Is(err, manager.ErrDisabled):
			http.Error(w, "stream is disabled", http.StatusConflict)
		case errors.Is(err, manager.ErrPreparing):
			http.Error(w, "stream source is preparing", http.StatusTooEarly)
		default:
			log.Error("on-demand acquire failed", "stream", req.Name, "err", err)
			http.Error(w, "on-demand activation failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /release", func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeDemandRequest(w, r)
		if !ok {
			return
		}
		mgr.ReleaseDemand(req.Name, req.Lease)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func decodeDemandRequest(w http.ResponseWriter, r *http.Request) (demandRequest, bool) {
	var req demandRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return demandRequest{}, false
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Lease = strings.TrimSpace(req.Lease)
	if !model.ValidName(req.Name) || req.Lease == "" || len(req.Lease) > 200 {
		http.Error(w, "invalid name or lease", http.StatusBadRequest)
		return demandRequest{}, false
	}
	return req, true
}

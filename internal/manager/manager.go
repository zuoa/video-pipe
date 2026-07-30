// Package manager owns the lifecycle of all ffmpeg stream processes: starting,
// stopping, restarting, restoring persisted state on boot, polling MediaMTX for
// online status, and producing status snapshots for the API/UI.
package manager

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"video-pipe/internal/ffmpeg"
	"video-pipe/internal/mediamtx"
	"video-pipe/internal/model"
	"video-pipe/internal/provider"
	"video-pipe/internal/store"
)

// pollInterval is how often MediaMTX online state is refreshed.
const pollInterval = 5 * time.Second

// Manager coordinates stream processes.
type Manager struct {
	store    *store.Store
	mtx      *mediamtx.Client
	mtxHost  string
	log      *slog.Logger

	ctx context.Context

	mu      sync.Mutex
	handles map[string]*entry
	wg      sync.WaitGroup
}

type entry struct {
	handle *ffmpeg.Handle
	stream model.Stream
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates a Manager. Call Start to boot.
func New(st *store.Store, mtx *mediamtx.Client, mtxHost string, log *slog.Logger) *Manager {
	return &Manager{
		store:   st,
		mtx:     mtx,
		mtxHost: mtxHost,
		log:     log,
		handles: make(map[string]*entry),
	}
}

// Start restores all streams whose desired state is "running" and launches the
// MediaMTX status poller. It returns once restore is complete; the poller runs
// until ctx is canceled. Use Wait to block until all processes have exited.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx

	streams, err := m.store.List(ctx)
	if err != nil {
		m.log.Error("manager: list streams for restore failed", "err", err)
	} else {
		for _, s := range streams {
			if s.DesiredState == model.StateRunning {
				if m.start(ctx, s) {
					m.log.Info("manager: restored stream", "name", s.Name)
				}
			}
		}
	}

	go m.pollLoop(ctx)
	return nil
}

// Wait blocks until every managed process goroutine has exited (used at shutdown).
func (m *Manager) Wait() { m.wg.Wait() }

// EnsureRunning starts (or replaces) the ffmpeg process for a stream and marks
// its desired state running. The stream must already exist in the store.
func (m *Manager) EnsureRunning(s model.Stream) error {
	if err := m.store.SetDesired(m.ctx, s.Name, model.StateRunning); err != nil {
		return err
	}
	m.start(m.ctx, s)
	return nil
}

// EnsureStopped stops the ffmpeg process for a stream (if running) and marks
// desired state stopped.
func (m *Manager) EnsureStopped(name string) error {
	m.stop(name)
	return m.store.SetDesired(m.ctx, name, model.StateStopped)
}

// Restart stops then starts a stream.
func (m *Manager) Restart(name string) error {
	s, err := m.store.Get(m.ctx, name)
	if err != nil {
		return err
	}
	m.stop(name)
	return m.EnsureRunning(s)
}

// Delete stops a stream and removes it from the store.
func (m *Manager) Delete(name string) error {
	m.stop(name)
	return m.store.Delete(m.ctx, name)
}

// HandleSnapshot returns the live process status for a stream, or (zero,false)
// if no process is currently running for it (e.g. it is stopped).
func (m *Manager) HandleSnapshot(name string) (ffmpeg.Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.handles[name]
	if !ok {
		return ffmpeg.Status{}, false
	}
	return e.handle.Snapshot(), true
}

// start launches a handle for s. If one already exists it is stopped first
// (replace). Returns true if a new process was started.
func (m *Manager) start(ctx context.Context, s model.Stream) bool {
	m.stop(s.Name) // ensure no stale handle for this name

	// For provider sources, build a resolver that refreshes the CDN URL before
	// every (re)start. Direct sources pass nil (they use s.SourceURL directly).
	var resolve ffmpeg.Resolver
	if s.Provider != "" {
		r, ok := provider.Get(s.Provider)
		if !ok {
			m.log.Error("manager: unknown provider", "stream", s.Name, "provider", s.Provider)
			return false
		}
		pageURL := s.SourceURL
		resolve = func(c context.Context) (string, map[string]string, bool, error) {
			res, err := r.Resolve(c, pageURL)
			if err != nil {
				return "", nil, false, err
			}
			return res.URL, res.Headers, res.Live, nil
		}
	}

	h := ffmpeg.NewHandle(s.Name, s, m.mtxHost, resolve, m.log.With("stream", s.Name))

	hctx, cancel := context.WithCancel(ctx)
	e := &entry{handle: h, stream: s, cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	m.handles[s.Name] = e
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(e.done)
		h.Run(hctx)
		m.log.Info("manager: process goroutine exited", "stream", s.Name)
	}()
	return true
}

// stop terminates the handle for name (if any) and waits for it to exit.
func (m *Manager) stop(name string) {
	m.mu.Lock()
	e, ok := m.handles[name]
	if ok {
		delete(m.handles, name)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	e.cancel()
	<-e.done
}

func (m *Manager) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *Manager) pollOnce(ctx context.Context) {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.handles))
	for _, e := range m.handles {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	for _, e := range entries {
		ps, err := m.mtx.GetPath(ctx, e.stream.Name)
		if err != nil {
			// Best-effort: keep the last cached value; never act on errors here.
			m.log.Debug("manager: mediamtx poll error", "stream", e.stream.Name, "err", err)
			continue
		}
		if ps == nil {
			e.handle.SetMediaMTX(false, 0)
		} else {
			e.handle.SetMediaMTX(ps.Online, ps.Readers)
		}
	}
}

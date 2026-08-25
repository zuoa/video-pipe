// Package manager owns on-demand ffmpeg processes, provider-cache preparation,
// MediaMTX status polling, and status snapshots for the API/UI.
package manager

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

const (
	prepareConcurrency = 2
	prepareRetryAfter  = 30 * time.Second
	demandLeaseTTL     = 45 * time.Second
)

var (
	// ErrDisabled means that a path exists but was explicitly disabled by the
	// user. A MediaMTX on-demand hook must not start it.
	ErrDisabled = errors.New("stream is disabled")
	// ErrPreparing means that a cached provider source is still being prepared.
	// The hook should retry while MediaMTX keeps the reader waiting.
	ErrPreparing = errors.New("stream source is preparing")
)

// Manager coordinates stream processes.
type Manager struct {
	store    *store.Store
	mtx      *mediamtx.Client
	mtxHost  string
	cacheDir string
	log      *slog.Logger

	ctx context.Context

	mu           sync.Mutex
	handles      map[string]*entry
	demands      map[string]map[string]time.Time
	prepares     map[string]*prepareEntry
	prepareSlots chan struct{}
	wg           sync.WaitGroup
}

type entry struct {
	handle *ffmpeg.Handle
	stream model.Stream
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// PreparationStatus describes ahead-of-time work that does not consume
// transcoding CPU. Today it is used for Bilibili VOD downloads.
type PreparationStatus struct {
	State     string
	LastError string
}

type prepareEntry struct {
	status  PreparationStatus
	cancel  context.CancelFunc
	done    chan struct{}
	retryAt time.Time
}

// New creates a Manager. Call Start to boot.
func New(st *store.Store, mtx *mediamtx.Client, mtxHost, cacheDir string, log *slog.Logger) *Manager {
	return &Manager{
		store:        st,
		mtx:          mtx,
		mtxHost:      mtxHost,
		cacheDir:     cacheDir,
		log:          log,
		handles:      make(map[string]*entry),
		demands:      make(map[string]map[string]time.Time),
		prepares:     make(map[string]*prepareEntry),
		prepareSlots: make(chan struct{}, prepareConcurrency),
	}
}

// Start restores lightweight preparation jobs and launches the MediaMTX status
// poller. It deliberately does not restore ffmpeg processes: enabled streams
// remain idle until MediaMTX reports an actual reader through its runOnDemand
// hook. Use Wait to block until all processes and preparation jobs have exited.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx

	streams, err := m.store.List(ctx)
	if err != nil {
		m.log.Error("manager: list streams for restore failed", "err", err)
	} else {
		for _, s := range streams {
			if s.DesiredState == model.StateRunning {
				m.Prepare(s)
			}
		}
	}

	go m.pollLoop(ctx)
	go m.leaseLoop(ctx)
	return nil
}

// Wait blocks until every ffmpeg and preparation goroutine has exited.
func (m *Manager) Wait() { m.wg.Wait() }

// Enable marks a stream available for on-demand playback. It does not start
// ffmpeg; MediaMTX's first reader will acquire a demand lease and start it.
func (m *Manager) Enable(s model.Stream) error {
	if err := m.store.SetDesired(m.ctx, s.Name, model.StateRunning); err != nil {
		return err
	}
	s.DesiredState = model.StateRunning
	m.Prepare(s)
	return nil
}

// EnsureStopped disables a stream, cancels preparation and stops its ffmpeg
// process even if a reader currently holds an on-demand lease.
func (m *Manager) EnsureStopped(name string) error {
	if err := m.store.SetDesired(m.ctx, name, model.StateStopped); err != nil {
		return err
	}
	m.cancelPreparation(name)
	m.stopAndClearDemand(name)
	return nil
}

// Delete stops a stream and removes it from the store.
func (m *Manager) Delete(name string) error {
	s, err := m.store.Get(m.ctx, name)
	if err != nil {
		return err
	}
	if err := m.store.Delete(m.ctx, name); err != nil {
		return err
	}
	m.cancelPreparation(name)
	m.stopAndClearDemand(name)
	if s.Provider == "bilibili" {
		for _, path := range []string{m.providerCachePath(s), m.providerCachePath(s) + ".part"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				m.log.Warn("remove provider cache failed", "stream", name, "path", path, "err", err)
			}
		}
	}
	return nil
}

// AcquireDemand records one MediaMTX runOnDemand command as a lease and starts
// the stream if this is its first reader. Re-acquiring the same lease is
// idempotent, which lets the hook heartbeat recover after a backend restart.
func (m *Manager) AcquireDemand(name, lease string) error {
	if !model.ValidName(name) || lease == "" {
		return fmt.Errorf("invalid demand lease")
	}
	s, err := m.store.Get(m.ctx, name)
	if err != nil {
		return err
	}
	if s.DesiredState != model.StateRunning {
		return ErrDisabled
	}

	if s.Provider == "bilibili" && provider.IsBilibiliVODURL(s.SourceURL) {
		m.Prepare(s)
		prep, ok := m.PreparationSnapshot(name)
		if !ok || prep.State != "ready" {
			return ErrPreparing
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	leases := m.demands[name]
	if leases == nil {
		leases = make(map[string]time.Time)
		m.demands[name] = leases
	}
	leases[lease] = time.Now()
	if _, ok := m.handles[name]; ok {
		return nil
	}
	e, err := m.newEntry(s)
	if err != nil {
		delete(leases, lease)
		if len(leases) == 0 {
			delete(m.demands, name)
		}
		return err
	}
	m.handles[name] = e
	m.launchLocked(e)
	m.log.Info("manager: stream activated on demand", "stream", name)
	return nil
}

// ReleaseDemand drops one reader lease. The last release stops ffmpeg but
// keeps desired_state=running, so the next reader can activate it again.
func (m *Manager) ReleaseDemand(name, lease string) {
	m.mu.Lock()
	leases := m.demands[name]
	if leases == nil {
		m.mu.Unlock()
		return
	}
	delete(leases, lease)
	if len(leases) != 0 {
		m.mu.Unlock()
		return
	}
	delete(m.demands, name)
	e := m.takeEntryLocked(name)
	m.mu.Unlock()
	m.stopEntry(name, e)
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

// PreparationSnapshot returns the current provider-cache preparation status.
func (m *Manager) PreparationSnapshot(name string) (PreparationStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.prepares[name]
	if !ok {
		return PreparationStatus{}, false
	}
	return p.status, true
}

// Prepare downloads a supported Bilibili VOD into persistent storage without
// launching ffmpeg. Calls are idempotent; failed downloads retry after a short
// backoff when a reader is still waiting.
func (m *Manager) Prepare(s model.Stream) {
	if s.Provider != "bilibili" || !provider.IsBilibiliVODURL(s.SourceURL) {
		return
	}
	path := m.providerCachePath(s)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		m.mu.Lock()
		m.prepares[s.Name] = &prepareEntry{status: PreparationStatus{State: "ready"}}
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	if current := m.prepares[s.Name]; current != nil {
		// A prior "ready" entry is stale when the stat above says the cache file
		// disappeared; recreate it instead of starting ffmpeg with a missing path.
		if current.status.State == "preparing" || time.Now().Before(current.retryAt) {
			m.mu.Unlock()
			return
		}
	}
	pctx, cancel := context.WithCancel(m.ctx)
	p := &prepareEntry{
		status: PreparationStatus{State: "preparing"},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.prepares[s.Name] = p
	m.wg.Add(1)
	go m.runPreparation(pctx, s, p)
	m.mu.Unlock()
}

func (m *Manager) runPreparation(ctx context.Context, s model.Stream, p *prepareEntry) {
	defer m.wg.Done()
	defer close(p.done)

	select {
	case m.prepareSlots <- struct{}{}:
		defer func() { <-m.prepareSlots }()
	case <-ctx.Done():
		return
	}

	r, ok := provider.Get("bilibili")
	if !ok {
		m.finishPreparation(s.Name, p, fmt.Errorf("bilibili provider unavailable"))
		return
	}
	res, err := r.Resolve(ctx, s.SourceURL)
	if err == nil && res.Live {
		err = fmt.Errorf("expected a Bilibili video, got a live source")
	}
	if err == nil {
		_, err = m.cacheProviderVOD(ctx, s, res)
	}
	if ctx.Err() != nil {
		return
	}
	m.finishPreparation(s.Name, p, err)
}

func (m *Manager) finishPreparation(name string, p *prepareEntry, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prepares[name] != p {
		return
	}
	p.cancel = nil
	if err != nil {
		p.status = PreparationStatus{State: "error", LastError: err.Error()}
		p.retryAt = time.Now().Add(prepareRetryAfter)
		m.log.Warn("manager: provider VOD preparation failed", "stream", name, "err", err)
		return
	}
	p.status = PreparationStatus{State: "ready"}
	p.retryAt = time.Time{}
	m.log.Info("manager: provider VOD ready; waiting for a reader", "stream", name)
}

// newEntry creates a not-yet-launched ffmpeg supervisor. Caller holds m.mu.
func (m *Manager) newEntry(s model.Stream) (*entry, error) {
	// For provider sources, build a resolver that refreshes the CDN URL before
	// every (re)start. Direct sources pass nil (they use s.SourceURL directly).
	var resolve ffmpeg.Resolver
	if s.Provider == "bilibili" && provider.IsBilibiliVODURL(s.SourceURL) {
		cachePath := m.providerCachePath(s)
		resolve = func(context.Context) (string, map[string]string, bool, error) {
			return cachePath, nil, false, nil
		}
	} else if s.Provider != "" {
		r, ok := provider.Get(s.Provider)
		if !ok {
			return nil, fmt.Errorf("unknown provider %q", s.Provider)
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

	hctx, cancel := context.WithCancel(m.ctx)
	e := &entry{handle: h, stream: s, ctx: hctx, cancel: cancel, done: make(chan struct{})}
	return e, nil
}

// launchLocked starts e and arranges for terminal supervisors to disappear so
// a still-active hook heartbeat can acquire a fresh retry budget. Caller holds
// m.mu and has already inserted e into m.handles.
func (m *Manager) launchLocked(e *entry) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(e.done)
		e.handle.Run(e.ctx)
		m.mu.Lock()
		if m.handles[e.stream.Name] == e {
			delete(m.handles, e.stream.Name)
		}
		m.mu.Unlock()
		m.log.Info("manager: process goroutine exited", "stream", e.stream.Name)
	}()
}

func (m *Manager) cacheProviderVOD(ctx context.Context, s model.Stream, res *provider.Result) (string, error) {
	path := m.providerCachePath(s)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		m.log.Info("using cached provider VOD", "stream", s.Name, "path", path, "bytes", info.Size())
		return path, nil
	}

	started := time.Now()
	m.log.Info("preparing provider VOD cache", "stream", s.Name, "path", path)
	n, err := provider.DownloadToFile(ctx, res.URL, res.Headers, path)
	if err != nil {
		return "", err
	}
	m.log.Info("provider VOD download complete",
		"stream", s.Name,
		"path", path,
		"bytes", n,
		"elapsed", time.Since(started).Round(time.Second),
	)
	return path, nil
}

func (m *Manager) providerCachePath(s model.Stream) string {
	sum := sha256.Sum256([]byte(s.SourceURL))
	name := fmt.Sprintf("%s-%x.media", s.Name, sum[:8])
	return filepath.Join(m.cacheDir, name)
}

// stopAndClearDemand terminates the handle and invalidates every outstanding
// hook lease for name.
func (m *Manager) stopAndClearDemand(name string) {
	m.mu.Lock()
	delete(m.demands, name)
	e := m.takeEntryLocked(name)
	m.mu.Unlock()
	m.stopEntry(name, e)
}

func (m *Manager) takeEntryLocked(name string) *entry {
	e := m.handles[name]
	delete(m.handles, name)
	return e
}

func (m *Manager) stopEntry(name string, e *entry) {
	if e == nil {
		return
	}
	e.cancel()
	<-e.done
	m.log.Info("manager: stream returned to idle", "stream", name)
}

func (m *Manager) cancelPreparation(name string) {
	m.mu.Lock()
	p := m.prepares[name]
	delete(m.prepares, name)
	m.mu.Unlock()
	if p == nil || p.cancel == nil {
		return
	}
	p.cancel()
	<-p.done
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

// leaseLoop is a fallback for abrupt MediaMTX/helper termination, where no
// release callback can be delivered. A healthy helper refreshes every 10s.
func (m *Manager) leaseLoop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.expireDemandLeases(now)
		}
	}
}

func (m *Manager) expireDemandLeases(now time.Time) {
	var expired []*entry
	m.mu.Lock()
	for name, leases := range m.demands {
		for lease, heartbeat := range leases {
			if now.Sub(heartbeat) >= demandLeaseTTL {
				delete(leases, lease)
			}
		}
		if len(leases) == 0 {
			delete(m.demands, name)
			if e := m.takeEntryLocked(name); e != nil {
				expired = append(expired, e)
			}
		}
	}
	m.mu.Unlock()
	for _, e := range expired {
		m.log.Warn("manager: expired stale on-demand lease", "stream", e.stream.Name)
		m.stopEntry(e.stream.Name, e)
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
		// A path cannot exist while ffmpeg is stopped, resolving, or waiting to
		// retry. Skipping the request also avoids MediaMTX logging a noisy API
		// 404 every five seconds for a stream that has already ended.
		if e.handle.Snapshot().State != ffmpeg.StateRunning {
			e.handle.SetMediaMTX(false, nil)
			continue
		}
		ps, err := m.mtx.GetPath(ctx, e.stream.Name)
		if err != nil {
			// Best-effort: keep the last cached value; never act on errors here.
			m.log.Debug("manager: mediamtx poll error", "stream", e.stream.Name, "err", err)
			continue
		}
		if ps == nil {
			e.handle.SetMediaMTX(false, nil)
		} else {
			e.handle.SetMediaMTX(ps.Online, ps.Readers)
		}
	}
}

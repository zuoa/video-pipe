package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// State is the lifecycle state of a managed stream.
type State string

const (
	StateRunning    State = "running"
	StateRestarting State = "restarting"
	StateError      State = "error"
	StateStopped    State = "stopped"
)

// Status is a point-in-time view of a Handle, safe to read from the API.
type Status struct {
	State         State     `json:"state"`
	RestartCount  int       `json:"restart_count"`
	LastError     string    `json:"last_error"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	MtxOnline     bool      `json:"mtx_online"`
	Readers       int       `json:"readers"`
}

// Supervision tunables.
const (
	staleTimeout   = 15 * time.Second // no ffmpeg output for this long => frozen source
	checkEvery     = 3 * time.Second
	maxRestarts    = 8                 // give up after this many consecutive failed restarts
	baseBackoff    = 2 * time.Second
	maxBackoff     = 60 * time.Second
	healthyRunFor  = 2 * maxBackoff    // a run this long resets the restart counter
	waitDelay      = 10 * time.Second  // SIGTERM->SIGKILL escalation for a stuck group
)

// exitKind classifies how an ffmpeg invocation ended.
type exitKind int

const (
	exitOK   exitKind = iota // clean exit (e.g. file EOF) — restart only for live sources
	exitFail                 // error — retry with backoff
)

// Handle supervises a single ffmpeg subprocess for one stream.
type Handle struct {
	name   string
	live   bool
	args   []string
	logger *slog.Logger

	mu            sync.RWMutex
	state         State
	restartCount  int
	lastError     string
	lastHeartbeat time.Time
	mtxOnline     bool
	readers       int

	lastOut atomic.Int64 // unix-nano timestamp of the last `progress=continue`
}

// NewHandle creates a supervisor for a stream.
func NewHandle(name string, live bool, args []string, logger *slog.Logger) *Handle {
	return &Handle{
		name:          name,
		live:          live,
		args:          args,
		logger:        logger,
		state:         StateRestarting,
		lastHeartbeat: time.Now(),
	}
}

// Run launches, watches, and restarts the ffmpeg process until ctx is canceled,
// the source ends cleanly (non-live only), or maxRestarts is exceeded.
func (h *Handle) Run(ctx context.Context) {
	defer h.setState(StateStopped)

	restarts := 0
	for {
		if ctx.Err() != nil {
			return
		}
		h.setState(StateRestarting)

		procCtx, procCancel := context.WithCancel(ctx)
		cmd, startedAt, err := h.start(procCtx)
		if err != nil {
			procCancel()
			h.setLastError(err.Error())
			if !h.retry(ctx, &restarts) {
				return
			}
			continue
		}

		h.setState(StateRunning)

		wdDone := make(chan struct{})
		go h.watchdog(procCtx, procCancel, wdDone)

		waitErr := cmd.Wait()
		close(wdDone)
		procCancel()
		killGroup(cmd) // sweep any lingering process-group members

		h.logger.Info("ffmpeg exited", "err", err2str(waitErr))

		switch {
		case ctx.Err() != nil:
			return // service shutdown / user stop
		case classify(waitErr) == exitOK && !h.live:
			h.logger.Info("source ended cleanly; stopping", "source", h.name)
			return // file finished — do not loop forever
		case time.Since(startedAt) >= healthyRunFor:
			restarts = 0 // ran healthy long enough; forgive prior failures
		}

		if waitErr != nil {
			h.setLastError(err2str(waitErr))
		}
		if !h.retry(ctx, &restarts) {
			return // exceeded max restarts -> state set to error by retry()
		}
	}
}

// retry records an attempt, optionally sleeps with backoff, and reports whether
// the loop should continue. It returns false (and sets state=error) when the
// attempt budget is exhausted or ctx is canceled.
func (h *Handle) retry(ctx context.Context, restarts *int) bool {
	*restarts++
	h.setRestartCount(*restarts)
	if *restarts > maxRestarts {
		h.setState(StateError)
		return false
	}
	delay := backoff(*restarts - 1)
	if !h.sleep(ctx, delay) {
		return false
	}
	return true
}

// start launches the ffmpeg process in its own process group and wires the
// progress/stderr pipe readers.
func (h *Handle) start(ctx context.Context) (*exec.Cmd, time.Time, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", h.args...)
	// Isolate ffmpeg (+ any children) in their own process group so we can kill
	// the whole group cleanly on stop/shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Graceful stop: SIGTERM the entire group when the context is done.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// Escalation: if the group ignores SIGTERM, exec force-kills after this.
	cmd.WaitDelay = waitDelay

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, time.Time{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, time.Time{}, err
	}

	if err := cmd.Start(); err != nil {
		return nil, time.Time{}, err
	}

	now := time.Now()
	h.lastOut.Store(now.UnixNano())

	// stdout: -progress key=value blocks; `progress=continue` = data flowing.
	go h.readProgress(stdout)
	// stderr: real log/error lines (warning level); keep the last as LastError.
	go h.readErrors(stderr)

	return cmd, now, nil
}

func (h *Handle) readProgress(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if sc.Text() == "progress=continue" {
			h.lastOut.Store(time.Now().UnixNano())
			h.setHeartbeat()
		}
	}
}

func (h *Handle) readErrors(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var last string
	for sc.Scan() {
		last = sc.Text()
		h.logger.Warn("ffmpeg", "msg", last)
	}
	if last != "" {
		h.setLastError(last)
	}
}

// watchdog kills the process when it is alive but producing no output for
// longer than staleTimeout (half-open TCP / hung camera).
func (h *Handle) watchdog(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			last := time.Unix(0, h.lastOut.Load())
			if d := time.Since(last); d > staleTimeout {
				h.logger.Warn("watchdog: no output, killing ffmpeg", "stale", d.Round(time.Second))
				cancel()
				return
			}
		}
	}
}

// Snapshot returns a point-in-time view of the handle.
func (h *Handle) Snapshot() Status {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Status{
		State:         h.state,
		RestartCount:  h.restartCount,
		LastError:     h.lastError,
		LastHeartbeat: h.lastHeartbeat,
		MtxOnline:     h.mtxOnline,
		Readers:       h.readers,
	}
}

// SetMediaMTX caches the latest MediaMTX-side state (online + reader count).
func (h *Handle) SetMediaMTX(online bool, readers int) {
	h.mu.Lock()
	h.mtxOnline = online
	h.readers = readers
	h.mu.Unlock()
}

func (h *Handle) setState(s State)       { h.mu.Lock(); h.state = s; h.mu.Unlock() }
func (h *Handle) setRestartCount(n int)  { h.mu.Lock(); h.restartCount = n; h.mu.Unlock() }
func (h *Handle) setLastError(s string)  { h.mu.Lock(); h.lastError = s; h.mu.Unlock() }
func (h *Handle) setHeartbeat()          { h.mu.Lock(); h.lastHeartbeat = time.Now(); h.mu.Unlock() }

func (h *Handle) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// classify maps an ffmpeg wait error to an exit kind. A canceled context is
// detected by the caller separately (ctx.Err()), so a signal/terminated error
// seen here implies a watchdog kill -> retry.
func classify(waitErr error) exitKind {
	if waitErr == nil {
		return exitOK
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		if ee.Success() {
			return exitOK
		}
		return exitFail
	}
	return exitFail
}

// backoff returns a full-jitter delay in [0, base*2^attempt) capped at max.
func backoff(attempt int) time.Duration {
	d := baseBackoff << uint(attempt)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// killGroup sweeps any surviving process-group members after Wait().
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH if already gone — ignored.
}

func err2str(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

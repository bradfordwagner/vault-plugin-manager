// Package health exposes the manager's liveness and readiness over HTTP so
// Kubernetes probes can tell a working manager from a wedged one.
//
// The manager serves no traffic, so "healthy" is defined by its reconcile loop
// rather than by request handling:
//
//   - liveness (/healthz) is a watchdog. The Runner declares, before it blocks
//     or before it starts a reconcile, when its next signal is due; liveness
//     fails once that deadline passes. A loop stuck on a hung exec, fetch, or
//     Vault call therefore fails the probe and the kubelet restarts the pod.
//   - readiness (/readyz) is a startup gate plus the Vault token. It flips true
//     on the first clean reconcile pass and then stays true for reconcile
//     errors — those are reported in the body without flapping the rollout —
//     but goes false while the Vault token has been invalid for longer than the
//     grace period, because a manager that cannot authenticate to Vault cannot
//     do its job.
//
// The ConfigMap watcher is checked two ways, because it fails two ways. An
// informer that has stopped outright fails liveness at once: the loop keeps
// reconciling its stale cache forever otherwise, looking perfectly healthy. A
// watch that is erroring but still relisting fails only readiness, and only
// after WatchGracePeriod, because client-go usually recovers on its own.
//
// Vault token state comes from the vault client's login/renew loop, which is
// authoritative for the token lifecycle. It is graced twice over: readiness
// tolerates TokenGracePeriod (short Vault restarts), liveness tolerates
// TokenFailTimeout (much longer, since restarting the pod does not fix a Vault
// that is down — it only re-runs the login the client already retries).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"vault-plugin-manager/internal/logging"
	"vault-plugin-manager/internal/metrics"
)

// Paths served by Handler. They match the chart's default probe paths.
const (
	LivenessPath  = "/healthz"
	ReadinessPath = "/readyz"
	// MetricsPath shares this server because the manager already runs one HTTP
	// listener for the probes and metrics need no separate lifecycle.
	MetricsPath = "/metrics"
)

// DefaultAddr is the default listen address for the probe server.
const DefaultAddr = ":8080"

// Config holds the health windows. StartupGrace must cover the whole startup
// path — the bounded Vault login retry and the informer cache sync — because
// the probe server comes up first, before any of it has run. The token windows
// are runtime tunables and can be replaced later with SetTokenGrace.
type Config struct {
	StartupGrace     time.Duration
	TokenGracePeriod time.Duration // readiness fails once the token is invalid this long
	TokenFailTimeout time.Duration // liveness fails once the token is invalid this long
	WatchGracePeriod time.Duration // readiness fails once the watch is erroring this long
}

// State is the health a probe reports on. It is safe for concurrent use: the
// reconcile Runner and the Vault client write it while the probe server reads it.
type State struct {
	mu        sync.Mutex
	now       func() time.Time // swapped in tests
	startedAt time.Time
	deadline  time.Time // liveness fails once now() passes this
	ready     bool
	lastPass  time.Time
	lastError string

	tokenValid     bool
	tokenInvalidAt time.Time // when the token first went invalid
	tokenError     string
	tokenGrace     time.Duration
	tokenFailAfter time.Duration

	// watcherCheck interrogates the ConfigMap informer on demand; nil until the
	// Runner has one. It must not call back into State.
	watcherCheck func() error
	watchErrorAt time.Time // when the watch FIRST started failing (never restarted by a repeat)
	watchLastErr time.Time // when the watch MOST RECENTLY failed
	watchError   string
	watchGrace   time.Duration
}

// New returns a State that stays live for cfg.StartupGrace without a heartbeat,
// and is neither ready nor holding a valid Vault token yet.
func New(cfg Config) *State {
	now := time.Now()
	return &State{
		now:       time.Now,
		startedAt: now,
		deadline:  now.Add(cfg.StartupGrace),
		// No token until the first login lands; the clock on that starts now, so
		// a Vault that never authenticates eventually fails both probes.
		tokenInvalidAt: now,
		tokenGrace:     cfg.TokenGracePeriod,
		tokenFailAfter: cfg.TokenFailTimeout,
		watchGrace:     cfg.WatchGracePeriod,
	}
}

// SetWatcherCheck installs the ConfigMap watcher's liveness check. A non-nil
// error from fn means the watcher has stopped and only a restart will fix it.
func (s *State) SetWatcherCheck(fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watcherCheck = fn
}

// SetWatchGrace replaces the watch-error window from the ConfigMap's settings.
func (s *State) SetWatchGrace(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchGrace = d
}

// WatchError records a broken ConfigMap watch. Like the token clock, the first
// failure starts it and later ones keep it running.
func (s *State) WatchError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Start a new failure EPISODE when the watch was last failing longer ago
	// than the grace: by then client-go would have reported again if it were
	// still broken, so this is a fresh problem and gets its own grace. Repeats
	// INSIDE the grace deliberately leave the clock alone -- a watch failing
	// every second must not hold the probe green by restarting it.
	if s.watchErrorAt.IsZero() || (!s.watchLastErr.IsZero() && s.now().Sub(s.watchLastErr) > s.watchGrace) {
		s.watchErrorAt = s.now()
	}
	s.watchLastErr = s.now()
	if err != nil {
		s.watchError = err.Error()
	}
}

// WatchHealthy records that the watcher delivered an event, which proves the
// watch (or the relist behind it) is working again.
func (s *State) WatchHealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchErrorAt = time.Time{}
	s.watchLastErr = time.Time{}
	s.watchError = ""
}

// SetTokenGrace replaces the token windows from the ConfigMap's settings.
func (s *State) SetTokenGrace(grace, failAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenGrace, s.tokenFailAfter = grace, failAfter
}

// TokenValid records a successful Vault login or token renewal.
func (s *State) TokenValid() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenValid = true
	s.tokenInvalidAt = time.Time{}
	s.tokenError = ""
}

// TokenInvalid records a failed Vault login or renewal. The first failure starts
// the grace clock; later failures keep it running rather than restarting it, so
// a login retrying every second cannot hold the probes green forever.
func (s *State) TokenInvalid(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenValid || s.tokenInvalidAt.IsZero() {
		s.tokenInvalidAt = s.now()
	}
	s.tokenValid = false
	if err != nil {
		s.tokenError = err.Error()
	}
}

// Heartbeat declares that the next signal is due within d. Liveness holds until
// then and fails after, so callers pass the longest they may legitimately take
// before checking in again.
func (s *State) Heartbeat(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = s.now().Add(d)
}

// ReconcileDone records the outcome of a reconcile pass. A nil error means a
// clean pass — either a successful reconcile or one the Runner legitimately
// skipped (absent or unparseable ConfigMap, which is the user's spec being
// wrong, not the manager being broken) — and marks the manager ready. A
// non-nil error is recorded for the probe body but never unreadies the pod.
func (s *State) ReconcileDone(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPass = s.now()
	if err != nil {
		s.lastError = err.Error()
		return
	}
	s.lastError = ""
	s.ready = true
}

// ReconcileSkipped records a pass the Runner declined to make because the
// ConfigMap was absent, empty, or unparseable. It counts as a clean pass -- the
// user's spec is wrong, not the manager -- so it never unreadies a pod that has
// been working. It does NOT open the startup gate: a manager whose ConfigMap has
// never parsed has reconciled nothing, and a rollout that gates on readiness
// must not go green on it.
func (s *State) ReconcileSkipped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPass = s.now()
}

// report is the probe response body: the same document on both endpoints, so a
// failing probe can be diagnosed by curling either one.
type report struct {
	Live            bool   `json:"live"`
	Ready           bool   `json:"ready"`
	Reason          string `json:"reason,omitempty"`
	Uptime          string `json:"uptime"`
	LastPass        string `json:"lastPass,omitempty"`
	LastError       string `json:"lastError,omitempty"`
	TokenValid      bool   `json:"tokenValid"`
	TokenInvalidFor string `json:"tokenInvalidFor,omitempty"`
	TokenError      string `json:"tokenError,omitempty"`
	WatcherRunning  bool   `json:"watcherRunning"`
	WatchFailingFor string `json:"watchFailingFor,omitempty"`
	WatchError      string `json:"watchError,omitempty"`
}

// snapshot renders the current state. Callers must not hold s.mu.
func (s *State) snapshot() report {
	// Interrogate the watcher outside the lock: it is another package's mutex,
	// and holding both would invite a deadlock.
	s.mu.Lock()
	check := s.watcherCheck
	s.mu.Unlock()
	var watcherErr error
	if check != nil {
		watcherErr = check()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	loopLive := !now.After(s.deadline)

	// How long the token has been invalid, and whether that has outrun each
	// window. A valid token is inside both by definition.
	var tokenDown time.Duration
	if !s.tokenValid {
		tokenDown = now.Sub(s.tokenInvalidAt)
	}
	tokenReady := s.tokenValid || tokenDown <= s.tokenGrace
	tokenLive := s.tokenValid || tokenDown <= s.tokenFailAfter

	// A watch that is erroring but still relisting is graced; a watcher that has
	// stopped is not, because nothing will restart it in-process.
	//
	// Failures must be ONGOING to fail readiness. client-go retries a broken
	// watch continuously, so a watch that is still down keeps calling
	// WatchError; one that recovered goes quiet. Without this second test a
	// single transient error would hold readiness down forever whenever the
	// recovery is a relist of an UNCHANGED ConfigMap, which delivers no event to
	// clear it. The FIRST-failure clock still decides the grace, so a watch
	// failing every second cannot hold the probes green by restarting it.
	var watchDown time.Duration
	if !s.watchErrorAt.IsZero() {
		watchDown = now.Sub(s.watchErrorAt)
	}
	watchFailing := !s.watchErrorAt.IsZero() && now.Sub(s.watchLastErr) <= s.watchGrace
	watchReady := !watchFailing || watchDown <= s.watchGrace
	watcherRunning := watcherErr == nil

	r := report{
		Live:           loopLive && tokenLive && watcherRunning,
		Ready:          s.ready && tokenReady && watchReady && watcherRunning,
		Uptime:         now.Sub(s.startedAt).Truncate(time.Second).String(),
		LastError:      s.lastError,
		TokenValid:     s.tokenValid,
		TokenError:     s.tokenError,
		WatcherRunning: watcherRunning,
	}
	// Only an ONGOING failure is reported: a recovered watch must not keep
	// showing an error the probe no longer counts.
	if watchFailing {
		r.WatchError = s.watchError
		r.WatchFailingFor = watchDown.Truncate(time.Second).String()
	}
	if watcherErr != nil && r.WatchError == "" {
		r.WatchError = watcherErr.Error()
	}
	if !s.lastPass.IsZero() {
		r.LastPass = s.lastPass.UTC().Format(time.RFC3339)
	}
	if !s.tokenValid {
		r.TokenInvalidFor = tokenDown.Truncate(time.Second).String()
	}
	switch {
	case !watcherRunning:
		r.Reason = "configmap watcher stopped: " + watcherErr.Error()
	case !loopLive:
		r.Reason = fmt.Sprintf("reconcile loop stalled: no heartbeat for %s", now.Sub(s.deadline).Truncate(time.Second))
	case !tokenLive:
		r.Reason = fmt.Sprintf("Vault token invalid for %s (limit %s)", tokenDown.Truncate(time.Second), s.tokenFailAfter)
	case !tokenReady:
		r.Reason = fmt.Sprintf("Vault token invalid for %s (grace %s)", tokenDown.Truncate(time.Second), s.tokenGrace)
	case !watchReady:
		r.Reason = fmt.Sprintf("configmap watch failing for %s (grace %s)", watchDown.Truncate(time.Second), s.watchGrace)
	case !s.ready:
		r.Reason = "waiting for the first successful reconcile"
	}
	return r
}

// Live reports whether the reconcile loop has checked in on time, the Vault
// token has not been invalid past TokenFailTimeout, and the ConfigMap watcher
// is still running.
func (s *State) Live() bool { return s.snapshot().Live }

// Ready reports whether the first clean reconcile pass has completed, the Vault
// token has not been invalid past TokenGracePeriod, and the ConfigMap watch has
// not been failing past WatchGracePeriod.
func (s *State) Ready() bool { return s.snapshot().Ready }

// TokenHealthy reports whether the Vault token is currently valid. It is the
// raw flag, ungraced — the grace windows are a probe concern, and a metric
// wants the underlying state. Named apart from TokenValid, which is the
// observer callback that records a successful login.
func (s *State) TokenHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenValid
}

// WatcherRunning reports whether the ConfigMap informer is still running. A
// State with no watcher check installed yet counts as running, matching
// snapshot's treatment of a nil check.
func (s *State) WatcherRunning() bool {
	// Interrogate outside the lock: watcherCheck takes another package's mutex.
	s.mu.Lock()
	check := s.watcherCheck
	s.mu.Unlock()
	return check == nil || check() == nil
}

// Handler serves the liveness and readiness endpoints for s. Both return the
// same JSON body, with 200 when the probed condition holds and 503 when it does
// not.
func Handler(s *State) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(LivenessPath, probe(s, func(r report) bool { return r.Live }))
	mux.Handle(ReadinessPath, probe(s, func(r report) bool { return r.Ready }))
	mux.Handle(MetricsPath, metrics.Handler())
	return mux
}

func probe(s *State, ok func(report) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r := s.snapshot()
		status := http.StatusOK
		if !ok(r) {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(r)
	})
}

// Serve starts the probe server on addr and returns a func that shuts it down.
// It binds synchronously, so a port conflict fails startup loudly instead of
// leaving the pod probe-less. The server also stops when ctx is cancelled.
func Serve(ctx context.Context, addr string, s *State) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("health: listening on %s: %w", addr, err)
	}
	return serve(ctx, ln, s), nil
}

// serve runs the probe server on an already-bound listener.
func serve(ctx context.Context, ln net.Listener, s *State) func() {
	srv := &http.Server{
		Handler: Handler(s),
		// Probes are trivial requests; a header timeout is enough to keep a
		// stuck client from pinning a connection.
		ReadHeaderTimeout: 5 * time.Second,
	}
	l := logging.Log().With("component", "health")
	l.With("addr", ln.Addr().String()).Info("probe server listening")

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.With("error", err).Error("probe server stopped")
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop
}

package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testCfg = Config{
	StartupGrace:     time.Minute,
	TokenGracePeriod: 2 * time.Minute,
	TokenFailTimeout: 15 * time.Minute,
	WatchGracePeriod: 2 * time.Minute,
}

// newTestState returns a State on a controllable clock with a valid Vault token
// — a manager past startup — plus the knob to advance the clock.
func newTestState(t *testing.T) (*State, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(testCfg)
	s.now = func() time.Time { return now }
	s.startedAt = now
	s.deadline = now.Add(testCfg.StartupGrace)
	s.tokenInvalidAt = now
	s.TokenValid()
	return s, func(d time.Duration) { now = now.Add(d) }
}

func TestLivenessIsAWatchdog(t *testing.T) {
	s, advance := newTestState(t)

	if !s.Live() {
		t.Fatal("want live inside the startup grace")
	}
	advance(61 * time.Second)
	if s.Live() {
		t.Fatal("want not live once the startup grace has passed")
	}

	// A heartbeat extends the deadline from now, reviving liveness.
	s.Heartbeat(10 * time.Minute)
	if !s.Live() {
		t.Fatal("want live after a heartbeat")
	}
	advance(9 * time.Minute)
	if !s.Live() {
		t.Fatal("want live inside the heartbeat window")
	}
	advance(2 * time.Minute)
	if s.Live() {
		t.Fatal("want not live past the heartbeat window")
	}
}

func TestReadinessGatesOnFirstCleanPass(t *testing.T) {
	s, _ := newTestState(t)

	if s.Ready() {
		t.Fatal("want not ready before the first reconcile")
	}
	s.ReconcileDone(errors.New("vault: 403"))
	if s.Ready() {
		t.Fatal("want not ready after a failed first reconcile")
	}
	s.ReconcileDone(nil)
	if !s.Ready() {
		t.Fatal("want ready after a clean pass")
	}

	// A later failure is reported but must not unready the pod.
	s.ReconcileDone(errors.New("vault: connection refused"))
	if !s.Ready() {
		t.Fatal("want still ready after a post-startup failure")
	}
	if got := s.snapshot().LastError; got != "vault: connection refused" {
		t.Fatalf("lastError = %q, want the recorded failure", got)
	}
}

// An invalid Vault token unreadies the pod after the grace period and only
// fails liveness after the much longer fail timeout.
func TestTokenGraceThenReadinessThenLiveness(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour) // keep the loop watchdog out of the way

	s.TokenInvalid(errors.New("vault: connection refused"))
	if !s.Ready() || !s.Live() {
		t.Fatal("want ready and live inside the token grace")
	}

	advance(3 * time.Minute) // past the 2m grace
	if s.Ready() {
		t.Fatal("want not ready once the token has been invalid past the grace")
	}
	if !s.Live() {
		t.Fatal("want still live: restarting the pod does not fix a down Vault")
	}
	if got := s.snapshot().Reason; got == "" {
		t.Fatal("want a reason naming the token")
	}

	advance(13 * time.Minute) // 16m total, past the 15m fail timeout
	if s.Live() {
		t.Fatal("want not live once the token has been invalid past the fail timeout")
	}

	// A successful re-login clears both immediately.
	s.TokenValid()
	if !s.Ready() || !s.Live() {
		t.Fatal("want ready and live again after re-authenticating")
	}
}

// Repeated failures must not restart the grace clock: a login retrying every
// second would otherwise hold the probes green forever.
func TestTokenInvalidDoesNotRestartTheGraceClock(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	s.TokenInvalid(errors.New("first"))
	for i := 0; i < 5; i++ {
		advance(time.Minute)
		s.TokenInvalid(errors.New("again"))
	}
	if s.Ready() {
		t.Fatal("want not ready: the token has been invalid for 5m, grace is 2m")
	}
	if got := s.snapshot().TokenInvalidFor; got != "5m0s" {
		t.Fatalf("tokenInvalidFor = %q, want 5m0s", got)
	}
}

// Before the first login the token is invalid, but the pod must stay live long
// enough for the client's bounded ignition retry to succeed.
func TestStartupHasNoTokenYetButStaysLive(t *testing.T) {
	s := New(testCfg)
	r := s.snapshot()
	if r.TokenValid {
		t.Fatal("want no valid token before the first login")
	}
	if !r.Live {
		t.Fatal("want live at startup")
	}
	if r.Ready {
		t.Fatal("want not ready at startup")
	}
}

func TestSetTokenGraceOverridesTheWindows(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)
	s.SetTokenGrace(30*time.Second, time.Minute)

	s.TokenInvalid(errors.New("boom"))
	advance(45 * time.Second)
	if s.Ready() {
		t.Fatal("want not ready past the 30s grace")
	}
	if !s.Live() {
		t.Fatal("want live inside the 1m fail timeout")
	}
	advance(30 * time.Second)
	if s.Live() {
		t.Fatal("want not live past the 1m fail timeout")
	}
}

// A watcher that has stopped outright fails liveness at once: nothing in-process
// restarts it, and the loop would otherwise reconcile its stale cache forever.
func TestStoppedWatcherFailsLivenessImmediately(t *testing.T) {
	s, _ := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	stopped := false
	s.SetWatcherCheck(func() error {
		if stopped {
			return errors.New("configmap informer stopped")
		}
		return nil
	})
	if !s.Live() || !s.Ready() {
		t.Fatal("want live and ready while the watcher runs")
	}

	stopped = true
	if s.Live() {
		t.Fatal("want not live once the watcher has stopped")
	}
	if s.Ready() {
		t.Fatal("want not ready once the watcher has stopped")
	}
	if got := s.snapshot().Reason; got != "configmap watcher stopped: configmap informer stopped" {
		t.Fatalf("reason = %q", got)
	}
}

// A watch that errors but keeps relisting is graced, and only unreadies the pod.
// client-go retries a broken watch continuously, so a watch that is still down
// keeps reporting; the failures below model that.
func TestWatchErrorsGraceThenUnready(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	s.WatchError(errors.New("connection refused"))
	if !s.Ready() {
		t.Fatal("want ready inside the watch grace")
	}

	for i := 0; i < 3; i++ { // 3m of continuous failure, past the 2m grace
		advance(time.Minute)
		s.WatchError(errors.New("connection refused"))
	}
	if s.Ready() {
		t.Fatal("want not ready once the watch has failed past the grace")
	}
	if !s.Live() {
		t.Fatal("want still live: client-go may yet relist its way out")
	}
	if got := s.snapshot().WatchFailingFor; got != "3m0s" {
		t.Fatalf("watchFailingFor = %q, want 3m0s", got)
	}

	// A delivered event proves the watch works again.
	s.WatchHealthy()
	if !s.Ready() {
		t.Fatal("want ready again after an event was delivered")
	}
	if got := s.snapshot().WatchError; got != "" {
		t.Fatalf("watchError = %q, want cleared", got)
	}
}

// Repeated failures must not restart the grace clock.
func TestWatchErrorDoesNotRestartTheGraceClock(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	s.WatchError(errors.New("first"))
	for i := 0; i < 4; i++ {
		advance(time.Minute)
		s.WatchError(errors.New("again"))
	}
	if s.Ready() {
		t.Fatal("want not ready: the watch has been failing for 4m, grace is 2m")
	}
	if got := s.snapshot().WatchFailingFor; got != "4m0s" {
		t.Fatalf("watchFailingFor = %q, want 4m0s", got)
	}
}

func TestSetWatchGraceOverridesTheWindow(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)
	s.SetWatchGrace(30 * time.Second)

	s.WatchError(errors.New("boom"))
	advance(20 * time.Second)
	s.WatchError(errors.New("boom"))
	advance(25 * time.Second)
	if s.Ready() {
		t.Fatal("want not ready past the 30s watch grace")
	}
}

// A watch that errors ONCE and then recovers must go ready again on its own.
// The recovery is often a relist of an unchanged ConfigMap, which delivers no
// event, so nothing calls WatchHealthy: without this, one transient apiserver
// blip would 503 readiness until somebody happened to edit the ConfigMap.
func TestWatchErrorClearsOnceFailuresStop(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	s.WatchError(errors.New("connection refused"))
	advance(3 * time.Minute) // no further failure reported: client-go relisted
	if !s.Ready() {
		t.Fatalf("want ready again once failures stopped; reason=%q", s.snapshot().Reason)
	}

	// ...and a watch that starts failing again is graced from its FIRST new
	// failure, not held against the old one.
	s.WatchError(errors.New("connection refused"))
	if !s.Ready() {
		t.Fatal("want ready inside the grace of the new failure")
	}
	for i := 0; i < 3; i++ {
		advance(time.Minute)
		s.WatchError(errors.New("connection refused"))
	}
	if s.Ready() {
		t.Fatal("want not ready: the watch has been failing continuously past the grace")
	}
}

func TestHandlerStatusCodes(t *testing.T) {
	s, advance := newTestState(t)
	h := Handler(s)

	// Startup: live, not ready.
	assertProbe(t, h, LivenessPath, http.StatusOK)
	assertProbe(t, h, ReadinessPath, http.StatusServiceUnavailable)

	// First clean pass: both ok.
	s.Heartbeat(5 * time.Minute)
	s.ReconcileDone(nil)
	assertProbe(t, h, LivenessPath, http.StatusOK)
	assertProbe(t, h, ReadinessPath, http.StatusOK)

	// Stalled loop: liveness fails, readiness holds.
	advance(6 * time.Minute)
	assertProbe(t, h, LivenessPath, http.StatusServiceUnavailable)
	assertProbe(t, h, ReadinessPath, http.StatusOK)
}

func TestHandlerBodyExplainsAStall(t *testing.T) {
	s, advance := newTestState(t)
	advance(3 * time.Minute)

	rec := httptest.NewRecorder()
	Handler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, LivenessPath, nil))

	var r report
	if err := json.NewDecoder(rec.Body).Decode(&r); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if r.Live {
		t.Fatal("want live=false in the body")
	}
	if r.Reason == "" {
		t.Fatal("want a reason explaining the stall")
	}
}

func TestServeAnswersProbesAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(testCfg)
	stop := serve(ctx, ln, s)

	url := "http://" + ln.Addr().String()
	if got := probeStatus(t, url+ReadinessPath); got != http.StatusServiceUnavailable {
		t.Fatalf("readiness before the first pass = %d, want 503", got)
	}
	s.TokenValid()
	s.ReconcileDone(nil)
	if got := probeStatus(t, url+ReadinessPath); got != http.StatusOK {
		t.Fatalf("readiness after a clean pass = %d, want 200", got)
	}

	stop()
	if _, err := http.Get(url + LivenessPath); err == nil {
		t.Fatal("want the probe server to stop serving after shutdown")
	}
}

func TestServeFailsLoudlyOnABadAddress(t *testing.T) {
	if _, err := Serve(context.Background(), "not-an-address", New(testCfg)); err == nil {
		t.Fatal("want an error binding an invalid address")
	}
}

func assertProbe(t *testing.T, h http.Handler, path string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != want {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("GET %s = %d, want %d (body: %s)", path, rec.Code, want, body)
	}
}

func probeStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TokenHealthy and WatcherRunning back the Prometheus gauges. They report the
// raw state, ungraced: the grace windows shape what the probes say, but a
// metric wants the underlying fact so a dashboard can show a token flapping
// inside its grace period.
func TestMetricAccessorsReportUngracedState(t *testing.T) {
	s, advance := newTestState(t)

	if !s.TokenHealthy() {
		t.Fatal("want a healthy token after a successful login")
	}
	s.TokenInvalid(errors.New("vault is sealed"))
	if s.TokenHealthy() {
		t.Error("want an unhealthy token immediately, without waiting out the grace")
	}
	// Still inside the grace window, so readiness holds while the gauge does not.
	if !s.Ready() && s.ready {
		t.Error("readiness should still be graced here; the accessor is the ungraced view")
	}
	s.TokenValid()
	if !s.TokenHealthy() {
		t.Error("want a healthy token again after the login recovers")
	}
	advance(time.Hour) // the token clock must not resurrect a valid token

	// No check installed yet reads as running, matching snapshot's nil handling.
	if !s.WatcherRunning() {
		t.Error("want WatcherRunning true before a check is installed")
	}
	stopped := false
	s.SetWatcherCheck(func() error {
		if stopped {
			return errors.New("configmap informer stopped")
		}
		return nil
	})
	if !s.WatcherRunning() {
		t.Error("want WatcherRunning true while the check passes")
	}
	stopped = true
	if s.WatcherRunning() {
		t.Error("want WatcherRunning false once the informer has stopped")
	}
}

// The metrics endpoint shares the probe server, so a request for it must be
// routed rather than 404ed by the probe mux.
func TestHandlerServesMetricsOnTheProbePort(t *testing.T) {
	s, _ := newTestState(t)
	rec := httptest.NewRecorder()
	Handler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetricsPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 from %s, got %d", MetricsPath, rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "vpm_") {
		t.Error("want vpm_ metrics in the body served on the probe port")
	}
}

// A skipped pass is clean -- the user's spec is wrong, not the manager -- but it
// is not proof that anything reconciled. A ConfigMap that has NEVER parsed must
// not open the startup gate, or `helm --wait` and a rollout both go green on a
// manager that has done nothing.
func TestSkippedPassDoesNotOpenTheStartupGate(t *testing.T) {
	s, _ := newTestState(t)
	s.Heartbeat(time.Hour)
	s.TokenValid()

	s.ReconcileSkipped()
	if s.Ready() {
		t.Fatal("want not ready: no reconcile has ever happened")
	}
	if got := s.snapshot().Reason; got != "waiting for the first successful reconcile" {
		t.Errorf("reason = %q", got)
	}
	if !s.Live() {
		t.Error("want live: a bad ConfigMap is not a wedged loop")
	}

	// Once a real pass lands, a later skip must not unready the pod: an edit that
	// breaks the ConfigMap should not tear a working manager out of service.
	s.ReconcileDone(nil)
	if !s.Ready() {
		t.Fatal("want ready after a real pass")
	}
	s.ReconcileSkipped()
	if !s.Ready() {
		t.Error("a skipped pass must not unready a manager that has been working")
	}
}

// Two unrelated blips must not merge into one failure episode. An apiserver
// rolling restart can produce a watch error, relist cleanly, and error again a
// minute later; because the ConfigMap did not change, no event is delivered to
// call WatchHealthy. Keying the episode boundary to the GRACE merged those into
// one episode dated from the first blip, which then unreadied a watch that had
// already recovered.
func TestSeparateWatchBlipsDoNotMerge(t *testing.T) {
	s, advance := newTestState(t)
	s.ReconcileDone(nil)
	s.Heartbeat(time.Hour)

	s.WatchError(errors.New("connection reset")) // t=0
	advance(90 * time.Second)
	s.WatchError(errors.New("connection reset")) // t=90s, a fresh episode
	advance(35 * time.Second)                    // t=125s: past the 2m grace measured from t=0

	if !s.Ready() {
		t.Fatalf("two separate blips were merged into one episode; reason=%q", s.snapshot().Reason)
	}

	// A watch that really is stuck still fails: client-go retries it on a
	// backoff capped around 30s, so the failures keep arriving.
	for i := 0; i < 6; i++ {
		advance(25 * time.Second)
		s.WatchError(errors.New("connection reset"))
	}
	if s.Ready() {
		t.Error("want not ready: the watch has been failing continuously past the grace")
	}
}

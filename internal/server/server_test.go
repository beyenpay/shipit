package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/deploy"
)

const testSecret = "0123456789abcdef0123456789abcdef"

type call struct{ action, project, tag string }

// fakeRunner records calls; behaviour is driven by err and gate.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	err   error
	gate  chan struct{} // if set, runs block until it is closed
	logf  func(string, ...any)
}

func (f *fakeRunner) run(action, project, tag string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{action, project, tag})
	f.mu.Unlock()
	f.logf("%s %s %s", action, project, tag)
	if f.gate != nil {
		<-f.gate
	}
	if tag == "" {
		tag = "v-prev"
	}
	return tag, f.err
}
func (f *fakeRunner) Deploy(_ context.Context, p, t string) (string, error) {
	return f.run("deploy", p, t)
}
func (f *fakeRunner) Rollback(_ context.Context, p, t string) (string, error) {
	return f.run("rollback", p, t)
}
func (f *fakeRunner) got() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

type fixture struct {
	t      *testing.T
	s      *Server
	ts     *httptest.Server
	runner *fakeRunner
	cfg    string
}

func writeConfig(t *testing.T, path, secret string) {
	t.Helper()
	y := fmt.Sprintf("secret: %q\nprojects:\n  web: {type: vite, repo: o/r, dir: /srv/web}\n  api: {type: go, repo: o/a, dir: /srv/api, service: api}\n", secret)
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, runner: &fakeRunner{}, cfg: filepath.Join(t.TempDir(), "shipit.yaml")}
	writeConfig(t, f.cfg, testSecret)
	f.s = New(f.cfg)
	f.s.NewRunner = func(_ *config.Config, logf func(string, ...any)) Runner {
		f.runner.logf = logf
		return f.runner
	}
	f.ts = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.ts.Close)
	return f
}

type resp struct {
	code int
	body []byte
	hdr  http.Header
}

func (r resp) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.body, &m)
	return m
}

func (r resp) str(key string) string {
	v, _ := r.json()[key].(string)
	return v
}

// do sends a correctly signed request.
func (f *fixture) do(method, path, query, body string) resp {
	f.t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return f.send(method, path, query, body, ts, Sign(testSecret, ts, method, path, []byte(body)))
}

func (f *fixture) send(method, path, query, body, ts, sig string) resp {
	f.t.Helper()
	u := f.ts.URL + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if ts != "" {
		req.Header.Set(HeaderTimestamp, ts)
	}
	if sig != "" {
		req.Header.Set(HeaderSignature, sig)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, b, res.Header}
}

func (f *fixture) waitJob(id string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := f.do("GET", "/v1/jobs/"+id, "", "")
		if r.code != 200 {
			f.t.Fatalf("job poll: %d %s", r.code, r.body)
		}
		if m := r.json(); m["status"] != "running" {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("job did not finish")
	return nil
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	r := f.send("GET", "/healthz", "", "", "", "")
	if r.code != 200 || r.str("status") != "ok" {
		t.Errorf("healthz: %d %s", r.code, r.body)
	}
}

func TestSignatureVector(t *testing.T) {
	// Independent check of the documented format; the composite action
	// computes the same value with openssl.
	got := Sign("k", "1700000000", "POST", "/v1/deploy", []byte(`{"a":1}`))
	if len(got) != 64 {
		t.Fatalf("signature length %d", len(got))
	}
	if got == Sign("k", "1700000000", "POST", "/v1/rollback", []byte(`{"a":1}`)) {
		t.Error("signature does not cover the path")
	}
	if got == Sign("k", "1700000000", "GET", "/v1/deploy", []byte(`{"a":1}`)) {
		t.Error("signature does not cover the method")
	}
	if got == Sign("k", "1700000001", "POST", "/v1/deploy", []byte(`{"a":1}`)) {
		t.Error("signature does not cover the timestamp")
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	body := `{"project":"web","tag":"v1"}`
	now := time.Now().Unix()
	ts := strconv.FormatInt(now, 10)
	good := Sign(testSecret, ts, "POST", "/v1/deploy", []byte(body))

	tests := []struct {
		name, ts, sig, path, body string
	}{
		{"no headers", "", "", "/v1/deploy", body},
		{"no signature", ts, "", "/v1/deploy", body},
		{"wrong secret", ts, Sign("other-secret-other-secret", ts, "POST", "/v1/deploy", []byte(body)), "/v1/deploy", body},
		{"not hex", ts, "zz", "/v1/deploy", body},
		{"tampered body", ts, good, "/v1/deploy", `{"project":"api","tag":"v1"}`},
		{"other path", ts, good, "/v1/rollback", body},
		{"stale timestamp", strconv.FormatInt(now-600, 10), Sign(testSecret, strconv.FormatInt(now-600, 10), "POST", "/v1/deploy", []byte(body)), "/v1/deploy", body},
		{"future timestamp", strconv.FormatInt(now+600, 10), Sign(testSecret, strconv.FormatInt(now+600, 10), "POST", "/v1/deploy", []byte(body)), "/v1/deploy", body},
		{"garbage timestamp", "abc", good, "/v1/deploy", body},
	}
	for _, tt := range tests {
		r := f.send("POST", tt.path, "", tt.body, tt.ts, tt.sig)
		if r.code != 401 {
			t.Errorf("%s: code %d, want 401 (%s)", tt.name, r.code, r.body)
		}
		if r.str("error") != "unauthorized" {
			t.Errorf("%s: error %q leaks detail", tt.name, r.str("error"))
		}
	}
	if len(f.runner.got()) != 0 {
		t.Error("an unauthenticated request ran a job")
	}

	if r := f.send("POST", "/v1/deploy", "", body, ts, good); r.code != 202 {
		t.Fatalf("valid request: %d %s", r.code, r.body)
	}
	if r := f.send("POST", "/v1/deploy", "", body, ts, good); r.code != 401 {
		t.Errorf("replayed request: %d, want 401", r.code)
	}
}

func TestDeployAsync(t *testing.T) {
	f := newFixture(t)
	f.runner.gate = make(chan struct{})

	r := f.do("POST", "/v1/deploy", "", `{"project":"web","tag":"v1.2.0"}`)
	if r.code != 202 || r.str("status") != "running" || r.str("job_id") == "" {
		t.Fatalf("enqueue: %d %s", r.code, r.body)
	}
	id := r.str("job_id")
	if loc := r.hdr.Get("Location"); loc != "/v1/jobs/"+id {
		t.Errorf("Location = %q", loc)
	}
	if m := f.do("GET", "/v1/jobs/"+id, "", "").json(); m["status"] != "running" {
		t.Errorf("status while blocked = %v", m["status"])
	}

	close(f.runner.gate)
	m := f.waitJob(id)
	if m["status"] != "success" || m["tag"] != "v1.2.0" || m["project"] != "web" || m["action"] != "deploy" {
		t.Errorf("finished job = %v", m)
	}
	if lines, _ := m["log"].([]any); len(lines) < 2 || !strings.Contains(fmt.Sprint(lines), "deploy web v1.2.0") {
		t.Errorf("log = %v", m["log"])
	}
	if m["finished_at"] == nil {
		t.Error("finished_at missing")
	}
	if got := f.runner.got(); len(got) != 1 || got[0] != (call{"deploy", "web", "v1.2.0"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestDeployAsyncFailure(t *testing.T) {
	f := newFixture(t)
	f.runner.err = errors.New("health check failed; rolled back to v1")
	r := f.do("POST", "/v1/deploy", "", `{"project":"api","tag":"v2"}`)
	m := f.waitJob(r.str("job_id"))
	if m["status"] != "failed" || !strings.Contains(fmt.Sprint(m["error"]), "rolled back to v1") {
		t.Errorf("job = %v", m)
	}
}

func TestWait(t *testing.T) {
	f := newFixture(t)
	r := f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v1"}`)
	if r.code != 200 || r.str("status") != "success" {
		t.Errorf("wait success: %d %s", r.code, r.body)
	}

	f.runner.err = errors.New("boom")
	r = f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v2"}`)
	if r.code != 500 || r.str("status") != "failed" || r.str("error") != "boom" {
		t.Errorf("wait failure: %d %s", r.code, r.body)
	}

	f.runner.err = fmt.Errorf("web: %w", deploy.ErrBusy)
	r = f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v3"}`)
	if r.code != 409 {
		t.Errorf("busy: %d %s", r.code, r.body)
	}
}

func TestRollback(t *testing.T) {
	f := newFixture(t)
	r := f.do("POST", "/v1/rollback", "wait=1", `{"project":"web"}`)
	if r.code != 200 || r.str("action") != "rollback" || r.str("tag") != "v-prev" {
		t.Errorf("rollback: %d %s", r.code, r.body)
	}
	r = f.do("POST", "/v1/rollback", "wait=true", `{"project":"web","tag":"v1"}`)
	if r.code != 200 {
		t.Errorf("rollback to tag: %d %s", r.code, r.body)
	}
	want := []call{{"rollback", "web", ""}, {"rollback", "web", "v1"}}
	got := f.runner.got()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, path, body string
		code             int
	}{
		{"unknown project", "/v1/deploy", `{"project":"nope","tag":"v1"}`, 404},
		{"missing tag", "/v1/deploy", `{"project":"web"}`, 400},
		{"bad tag", "/v1/deploy", `{"project":"web","tag":"../x"}`, 400},
		{"bad tag rollback", "/v1/rollback", `{"project":"web","tag":"a b"}`, 400},
		{"unknown field", "/v1/deploy", `{"project":"web","tag":"v1","cmd":"rm -rf /"}`, 400},
		{"not json", "/v1/deploy", `project=web`, 400},
		{"empty body", "/v1/deploy", ``, 400},
	}
	for _, tt := range tests {
		if r := f.do("POST", tt.path, "", tt.body); r.code != tt.code {
			t.Errorf("%s: %d, want %d (%s)", tt.name, r.code, tt.code, r.body)
		}
	}
	if len(f.runner.got()) != 0 {
		t.Error("an invalid request started a job")
	}
}

func TestRoutes(t *testing.T) {
	f := newFixture(t)
	if r := f.do("GET", "/v1/jobs/deadbeef", "", ""); r.code != 404 || !strings.Contains(r.str("error"), "shipit restarts") {
		t.Errorf("unknown job: %d %s", r.code, r.body)
	}
	if r := f.do("GET", "/v1/deploy", "", ""); r.code != 405 {
		t.Errorf("GET /v1/deploy: %d", r.code)
	}
	if r := f.do("GET", "/nope", "", ""); r.code != 404 {
		t.Errorf("unknown route: %d", r.code)
	}
}

func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	r := f.do("POST", "/v1/deploy", "", `{"project":"web","tag":"`+strings.Repeat("a", maxBodyBytes)+`"}`)
	if r.code != 413 {
		t.Errorf("code %d, want 413", r.code)
	}
}

func TestConfigReloadedPerRequest(t *testing.T) {
	f := newFixture(t)
	if r := f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v1"}`); r.code != 200 {
		t.Fatalf("before: %d", r.code)
	}

	writeConfig(t, f.cfg, "a-brand-new-secret-a-brand-new")
	if r := f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v2"}`); r.code != 401 {
		t.Errorf("old secret still accepted: %d", r.code)
	}

	if err := os.WriteFile(f.cfg, []byte("projects: [not valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := f.do("POST", "/v1/deploy", "wait=true", `{"project":"web","tag":"v3"}`)
	if r.code != 500 || strings.Contains(string(r.body), "yaml") {
		t.Errorf("broken config: %d %s", r.code, r.body)
	}
	if n := len(f.runner.got()); n != 1 {
		t.Errorf("jobs run = %d, want 1", n)
	}

	if err := os.WriteFile(f.cfg, []byte("secret: short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.do("GET", "/v1/jobs/x", "", ""); r.code != 500 {
		t.Errorf("weak secret: %d", r.code)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.s.limit.rate = 0.001
	f.s.limit.burst = 3
	codes := map[int]int{}
	for i := 0; i < 6; i++ {
		codes[f.send("GET", "/healthz", "", "", "", "").code]++
	}
	if codes[200] != 3 || codes[429] != 3 {
		t.Errorf("codes = %v, want 3x200 and 3x429", codes)
	}
}

func TestAuthFailureLockout(t *testing.T) {
	f := newFixture(t)
	body := `{"project":"web","tag":"v1"}`
	for i := 0; i < maxAuthFailures; i++ {
		if r := f.send("POST", "/v1/deploy", "", body, strconv.FormatInt(time.Now().Unix(), 10), "00"); r.code != 401 {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	// The next request is refused outright, even with a valid signature...
	if r := f.do("POST", "/v1/deploy", "", body); r.code != 429 {
		t.Errorf("after lockout: %d, want 429", r.code)
	}
	if len(f.runner.got()) != 0 {
		t.Error("locked-out request ran a job")
	}
	// ...until the failure window is over.
	base := time.Now()
	f.s.limit.now = func() time.Time { return base.Add(failureWindow + time.Second) }
	if r := f.do("POST", "/v1/deploy", "", body); r.code != 202 {
		t.Errorf("after window: %d, want 202", r.code)
	}
}

func TestLimiterIsPerIP(t *testing.T) {
	l := newLimiter()
	l.burst = 1
	l.rate = 0.001
	if !l.allow("1.1.1.1") || l.allow("1.1.1.1") {
		t.Error("bucket for 1.1.1.1 misbehaves")
	}
	if !l.allow("2.2.2.2") {
		t.Error("one IP exhausted another's allowance")
	}
	for i := 0; i < maxAuthFailures; i++ {
		l.fail("3.3.3.3")
	}
	if l.allow("3.3.3.3") {
		t.Error("IP with too many failures allowed")
	}
	if !l.allow("4.4.4.4") {
		t.Error("lockout leaked to another IP")
	}
}

func TestLimiterEvictsIdleIPs(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < maxTrackedIPs; i++ {
		l.allow(strconv.Itoa(i))
	}
	if l.allow("newcomer") {
		t.Error("table full of active IPs should fail closed")
	}
	now = now.Add(idleIPAfter + time.Second)
	if !l.allow("newcomer") {
		t.Error("idle IPs were not evicted")
	}
}

func TestReplayCache(t *testing.T) {
	c := newReplayCache()
	now := time.Now()
	if !c.add("a", now) || c.add("a", now) {
		t.Error("duplicate not detected")
	}
	if !c.add("a", now.Add(2*maxClockSkew+2*time.Second)) {
		t.Error("entry did not expire")
	}
}

func TestJobPrune(t *testing.T) {
	s := newJobStore()
	base := time.Now()
	s.now = func() time.Time { return base }
	run := func(context.Context, func(string, ...any)) (string, error) { return "v", nil }

	old := s.start("p", "deploy", "v", run)
	<-old.done
	s.wg.Wait()
	s.now = func() time.Time { return base.Add(jobRetention + time.Minute) }
	fresh := s.start("p", "deploy", "v", run)
	<-fresh.done
	if s.get(old.ID) != nil {
		t.Error("expired job kept")
	}
	if s.get(fresh.ID) == nil {
		t.Error("fresh job dropped")
	}

	for i := 0; i < maxJobs+20; i++ {
		j := s.start("p", "deploy", "v", run)
		<-j.done
	}
	s.mu.Lock()
	n := len(s.jobs)
	s.mu.Unlock()
	if n > maxJobs {
		t.Errorf("%d jobs kept, cap is %d", n, maxJobs)
	}
}

func TestRunShutsDownGracefully(t *testing.T) {
	f := newFixture(t)
	f.runner.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, "127.0.0.1:0") }()

	// Start a job through the handler, then ask the server to stop: Run must
	// wait for the job.
	r := f.do("POST", "/v1/deploy", "", `{"project":"web","tag":"v1"}`)
	if r.code != 202 {
		t.Fatalf("enqueue: %d", r.code)
	}
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while a job was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(f.runner.gate)
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the job finished")
	}
}

func TestRunListenError(t *testing.T) {
	f := newFixture(t)
	if err := f.s.Run(context.Background(), "256.0.0.1:99999"); err == nil {
		t.Error("expected listen error")
	}
}

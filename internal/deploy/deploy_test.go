package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/release"
)

type tf struct {
	name string
	body string
	mode int64
}

func tgz(t *testing.T, files []tf) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// elfBin returns a minimal ELF64 little-endian header for the given machine.
func elfBin(arch string) string {
	machine := map[string]uint16{"amd64": 62, "arm64": 183}[arch]
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], machine)
	return string(b)
}

func pkg(t *testing.T, typ, arch string) []byte {
	switch typ {
	case "go":
		return tgz(t, []tf{{name: "app", body: elfBin(arch), mode: 0o755}})
	case "next":
		return tgz(t, []tf{{name: "server.js", body: "//"}, {name: ".next/static/a.js", body: "//"}, {name: "public/x", body: "x"}})
	default:
		return tgz(t, []tf{{name: "index.html", body: "<html>"}, {name: "assets/a.js", body: "//"}})
	}
}

type fakeSource struct {
	archives  map[string][]byte
	latest    string
	releases  int
	downloads []string
}

func (f *fakeSource) Release(ctx context.Context, repo, tag string) (*release.Release, error) {
	f.releases++
	if tag == "" {
		tag = f.latest
	}
	if _, ok := f.archives[tag]; !ok {
		return nil, fmt.Errorf("no release %s", tag)
	}
	return &release.Release{Tag: tag}, nil
}

func (f *fakeSource) DownloadVerified(ctx context.Context, repo string, rel *release.Release, name, dst string) error {
	f.downloads = append(f.downloads, name)
	return os.WriteFile(dst, f.archives[rel.Tag], 0o600)
}

type env struct {
	t        *testing.T
	dir      string
	cfg      *config.Config
	d        *Deployer
	src      *fakeSource
	restarts []string // tag that current pointed to at each restart
	failTag  string   // the release whose health check fails
	bad      atomic.Bool
}

func setup(t *testing.T, typ string, tags ...string) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), src: &fakeSource{archives: map[string][]byte{}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.bad.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)

	svc := ""
	if typ != "vite" {
		svc = ", service: app"
	}
	y := fmt.Sprintf(`projects: {app: {type: %s, repo: o/r, dir: "%s"%s, health: "%s/", health_timeout: 300ms, keep: 3}}`,
		typ, e.dir, svc, srv.URL)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg
	for _, tag := range tags {
		e.src.archives[tag] = pkg(t, typ, "amd64")
	}
	e.d = &Deployer{
		Cfg:          cfg,
		Arch:         "amd64",
		Source:       func(string) Source { return e.src },
		HTTP:         srv.Client(),
		PollInterval: 10 * time.Millisecond,
		Logf:         t.Logf,
		Restart: func(ctx context.Context, service string) error {
			tag, err := readTag(cfg.Projects["app"], currentName)
			if err != nil {
				return err
			}
			e.restarts = append(e.restarts, tag)
			e.bad.Store(tag == e.failTag)
			return nil
		},
	}
	return e
}

func (e *env) deploy(tag string) string {
	e.t.Helper()
	got, err := e.d.Deploy(context.Background(), "app", tag)
	if err != nil {
		e.t.Fatalf("Deploy(%q): %v", tag, err)
	}
	return got
}

func (e *env) links(cur, prev string) {
	e.t.Helper()
	p := e.cfg.Projects["app"]
	gc, err := readTag(p, currentName)
	if err != nil {
		e.t.Fatal(err)
	}
	gp, err := readTag(p, previousName)
	if err != nil {
		e.t.Fatal(err)
	}
	if gc != cur || gp != prev {
		e.t.Errorf("links: current=%q previous=%q, want current=%q previous=%q", gc, gp, cur, prev)
	}
}

func (e *env) tags() []string {
	e.t.Helper()
	tags, err := e.d.Releases("app")
	if err != nil {
		e.t.Fatal(err)
	}
	return tags
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want it to contain %q", err, substr)
	}
}

func TestDeployVite(t *testing.T) {
	e := setup(t, "vite", "v1", "v2")
	e.deploy("v1")
	e.links("v1", "")
	e.deploy("v2")
	e.links("v2", "v1")

	if want := []string{"app-v1-any.tar.gz", "app-v2-any.tar.gz"}; !reflect.DeepEqual(e.src.downloads, want) {
		t.Errorf("downloads = %v, want %v", e.src.downloads, want)
	}
	if len(e.restarts) != 0 {
		t.Errorf("vite must not restart anything, got %v", e.restarts)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "current", "index.html")); err != nil {
		t.Error(err)
	}
}

func TestDeployGoChecks(t *testing.T) {
	e := setup(t, "go", "v1")

	e.src.archives["v1"] = pkg(t, "go", "arm64")
	_, err := e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "EM_AARCH64")
	if _, err := os.Stat(filepath.Join(e.dir, "releases", "v1")); err == nil {
		t.Error("rejected release was installed")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "releases")); len(entries) != 0 {
		t.Errorf("leftovers after rejected release: %v", entries)
	}
	e.links("", "")

	e.src.archives["v1"] = tgz(t, []tf{{name: "app", body: elfBin("amd64"), mode: 0o644}})
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "not executable")

	e.src.archives["v1"] = tgz(t, []tf{{name: "app", body: "#!/bin/sh\n", mode: 0o755}})
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "not an ELF")

	e.src.archives["v1"] = tgz(t, []tf{{name: "other", body: elfBin("amd64"), mode: 0o755}})
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "missing app")

	e.src.archives["v1"] = pkg(t, "go", "amd64")
	e.src.downloads = nil
	e.deploy("v1")
	e.links("v1", "")
	if want := []string{"app-v1-linux-amd64.tar.gz"}; !reflect.DeepEqual(e.src.downloads, want) {
		t.Errorf("downloads = %v, want %v", e.src.downloads, want)
	}
	if want := []string{"v1"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}
}

func TestDeployNextChecks(t *testing.T) {
	e := setup(t, "next", "v1")
	e.src.archives["v1"] = tgz(t, []tf{{name: "server.js", body: "//"}})
	_, err := e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, ".next/static")

	e.src.archives["v1"] = tgz(t, []tf{{name: ".next/static/a.js", body: "//"}})
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "server.js")

	e.src.archives["v1"] = pkg(t, "next", "amd64")
	e.deploy("v1")
	e.links("v1", "")
}

func TestHealthFailureRollsBack(t *testing.T) {
	e := setup(t, "go", "v1", "v2")
	e.failTag = "v2"
	e.deploy("v1")
	_, err := e.d.Deploy(context.Background(), "app", "v2")
	wantErr(t, err, "rolled back to v1")

	e.links("v1", "")
	if want := []string{"v1", "v2", "v1"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}
	if !hasRelease(e.cfg.Projects["app"], "v2") {
		t.Error("failed release should stay on disk for inspection")
	}
}

func TestRollbackKeepsPreviousOfPreviousState(t *testing.T) {
	e := setup(t, "go", "v1", "v2", "v3")
	e.deploy("v1")
	e.deploy("v2")
	e.failTag = "v3"
	_, err := e.d.Deploy(context.Background(), "app", "v3")
	wantErr(t, err, "rolled back to v2")
	e.links("v2", "v1")
}

func TestFirstDeployUnhealthy(t *testing.T) {
	e := setup(t, "go", "v1")
	e.failTag = "v1"
	_, err := e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "no earlier version")
	e.links("v1", "")
	if want := []string{"v1"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}
}

func TestRollbackSurvivesCancelledContext(t *testing.T) {
	e := setup(t, "go", "v1", "v2")
	e.failTag = "v2"
	e.deploy("v1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := e.d.Restart
	e.d.Restart = func(c context.Context, s string) error {
		err := orig(c, s)
		if e.restarts[len(e.restarts)-1] == "v2" {
			cancel() // the caller gives up while v2 is being health-checked
		}
		return err
	}
	_, err := e.d.Deploy(ctx, "app", "v2")
	wantErr(t, err, "rolled back to v1")
	e.links("v1", "")
	if want := []string{"v1", "v2", "v1"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}
}

func TestRollback(t *testing.T) {
	e := setup(t, "go", "v1", "v2")
	ctx := context.Background()

	_, err := e.d.Rollback(ctx, "app", "")
	wantErr(t, err, "no previous version")

	e.deploy("v1")
	e.deploy("v2")
	e.restarts = nil

	got, err := e.d.Rollback(ctx, "app", "")
	if err != nil || got != "v1" {
		t.Fatalf("Rollback = %q, %v", got, err)
	}
	e.links("v1", "v2")
	if _, err := e.d.Rollback(ctx, "app", ""); err != nil {
		t.Fatal(err)
	}
	e.links("v2", "v1")
	if _, err := e.d.Rollback(ctx, "app", "v1"); err != nil {
		t.Fatal(err)
	}
	e.links("v1", "v2")
	if want := []string{"v1", "v2", "v1"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}

	_, err = e.d.Rollback(ctx, "app", "v9")
	wantErr(t, err, "not on this server")
	_, err = e.d.Rollback(ctx, "app", "v1")
	wantErr(t, err, "already the current")
	if len(e.src.downloads) != 2 {
		t.Errorf("rollback must not download, downloads = %v", e.src.downloads)
	}
}

func TestDeployExistingLocalSkipsDownload(t *testing.T) {
	e := setup(t, "go", "v1", "v2")
	e.deploy("v1")
	e.deploy("v2")
	calls, downloads := e.src.releases, len(e.src.downloads)

	e.deploy("v1")
	e.links("v1", "v2")
	if e.src.releases != calls || len(e.src.downloads) != downloads {
		t.Errorf("deploying a local tag used the network (release calls %d->%d, downloads %d->%d)",
			calls, e.src.releases, downloads, len(e.src.downloads))
	}
}

func TestDeployCurrentAgainRestarts(t *testing.T) {
	e := setup(t, "go", "v1", "v2")
	e.deploy("v1")
	e.deploy("v2")
	e.restarts = nil
	e.deploy("v2")
	e.links("v2", "v1")
	if want := []string{"v2"}; !reflect.DeepEqual(e.restarts, want) {
		t.Errorf("restarts = %v, want %v", e.restarts, want)
	}
}

func TestDeployLatest(t *testing.T) {
	e := setup(t, "vite", "v1", "v2")
	e.src.latest = "v2"
	if got := e.deploy(""); got != "v2" {
		t.Errorf("deployed %q, want v2", got)
	}
	e.links("v2", "")

	e.src.archives["v1/2"] = nil
	e.src.latest = "v1/2"
	_, err := e.d.Deploy(context.Background(), "app", "")
	wantErr(t, err, "cannot be deployed")
}

func TestKeepCleanup(t *testing.T) {
	e := setup(t, "vite", "v1", "v2", "v3", "v4")
	e.deploy("v1")
	e.deploy("v2")
	e.deploy("v3")
	if got := len(e.tags()); got != 3 {
		t.Fatalf("expected 3 releases, got %v", e.tags())
	}
	now := time.Now()
	for i, tag := range []string{"v1", "v2", "v3"} {
		old := now.Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(filepath.Join(e.dir, "releases", tag), old, old); err != nil {
			t.Fatal(err)
		}
	}

	e.deploy("v4") // keep=3: v4 (current), v3 (previous) and the newest other, v2
	if want := []string{"v4", "v3", "v2"}; !reflect.DeepEqual(e.tags(), want) {
		t.Errorf("releases = %v, want %v", e.tags(), want)
	}

	e.cfg.Projects["app"].Keep = 2
	e.deploy("v3")
	if want := []string{"v3", "v4"}; !reflect.DeepEqual(sorted(e.tags()), want) {
		t.Errorf("releases = %v, want current v3 and previous v4 only", e.tags())
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestBusy(t *testing.T) {
	e := setup(t, "vite", "v1")
	l, err := acquire(filepath.Join(e.dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	_, err = e.d.Rollback(context.Background(), "app", "")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("rollback err = %v, want ErrBusy", err)
	}
	l.release()
	e.deploy("v1")
}

func TestStaleLeftoversRemoved(t *testing.T) {
	e := setup(t, "vite", "v1")
	rd := filepath.Join(e.dir, "releases")
	if err := os.MkdirAll(filepath.Join(rd, ".tmp-v1", "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rd, ".dl-v1.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.deploy("v1")
	entries, _ := os.ReadDir(rd)
	if len(entries) != 1 || entries[0].Name() != "v1" {
		t.Errorf("releases dir = %v", entries)
	}
}

func TestForeignCurrentIsRefused(t *testing.T) {
	e := setup(t, "vite", "v1")
	if err := os.Mkdir(filepath.Join(e.dir, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "not a symlink")
	if len(e.src.downloads) != 0 {
		t.Error("downloaded although current is unusable")
	}

	if err := os.Remove(filepath.Join(e.dir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(e.dir, "current")); err != nil {
		t.Fatal(err)
	}
	_, err = e.d.Deploy(context.Background(), "app", "v1")
	wantErr(t, err, "not a shipit release")
}

func TestInputErrors(t *testing.T) {
	e := setup(t, "vite", "v1")
	ctx := context.Background()
	_, err := e.d.Deploy(ctx, "nope", "v1")
	wantErr(t, err, `unknown project "nope"`)
	for _, tag := range []string{"../x", "a/b", ".hidden"} {
		_, err = e.d.Deploy(ctx, "app", tag)
		wantErr(t, err, "invalid tag")
		_, err = e.d.Rollback(ctx, "app", tag)
		wantErr(t, err, "invalid tag")
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	_, err = e.d.Deploy(ctx, "app", "v1")
	wantErr(t, err, "does not exist")
}

func TestStatusAndReleases(t *testing.T) {
	e := setup(t, "vite", "v1", "v2")
	e.deploy("v1")
	e.deploy("v2")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(e.dir, "releases", "v1"), old, old); err != nil {
		t.Fatal(err)
	}
	st, err := e.d.Status("app")
	if err != nil || st != (Status{Current: "v2", Previous: "v1"}) {
		t.Errorf("Status = %+v, %v", st, err)
	}
	if want := []string{"v2", "v1"}; !reflect.DeepEqual(e.tags(), want) {
		t.Errorf("Releases = %v, want %v", e.tags(), want)
	}
}

func TestWaitHealthyRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	d := &Deployer{HTTP: srv.Client(), PollInterval: 5 * time.Millisecond}
	p := &config.Project{Health: srv.URL, HealthTimeout: 2 * time.Second}
	if err := d.waitHealthy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 4 {
		t.Errorf("hits = %d, want 4", hits.Load())
	}

	hits.Store(-1000)
	p.HealthTimeout = 100 * time.Millisecond
	wantErr(t, d.waitHealthy(context.Background(), p), "not healthy after")
}

func TestSystemctlRestartRejectsOptions(t *testing.T) {
	for _, s := range []string{"", "--now", "-x"} {
		wantErr(t, SystemctlRestart(context.Background(), s), "invalid service")
	}
}

package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beyenpay/shipit/internal/config"
)

type fakeRemote struct {
	expires  string
	tokenErr error
	repoErr  map[string]error
}

func (f fakeRemote) TokenStatus(context.Context) (string, error) { return f.expires, f.tokenErr }
func (f fakeRemote) CheckRepo(_ context.Context, repo string) error {
	return f.repoErr[repo]
}

// sys fakes systemctl/sudo output per command line.
type sys map[string]struct {
	out string
	err error
}

func (s sys) run(_ context.Context, name string, args ...string) (string, error) {
	r, ok := s[name+" "+strings.Join(args, " ")]
	if !ok {
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	return r.out, r.err
}

// showAPI is the command check runs to inspect the unit of the api project.
const showAPI = "systemctl show api -p LoadState -p User -p WorkingDirectory -p EnvironmentFiles"

// fakeRunner mirrors the rules of deploy.ValidateRunner without looking at the
// accounts of the machine the tests run on.
func fakeRunner(user string) error {
	switch user {
	case "":
		return errors.New("unit has no User= set, so it would run as root")
	case "root":
		return errors.New("unit runs as root")
	}
	return nil
}

type rig struct {
	dir string
	cfg string
	sys sys
	rem fakeRemote
	// open maps a file to the error opening it as the shipit user gives. Files
	// that are not listed are not readable.
	open map[string]error
}

func (r *rig) checker(t *testing.T, y string, offline bool) *Checker {
	t.Helper()
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, r.cfg, offline)
	c.Run = r.sys.run
	c.Runner = fakeRunner
	c.Open = func(path string) error {
		if err, ok := r.open[path]; ok {
			return err
		}
		return fs.ErrPermission
	}
	c.Probe = func(context.Context, string) (PortState, string) { return PortShipit, "v1.2.0" }
	c.Listen = func(string) error { return nil }
	c.Remote = func(string) Remote { return r.rem }
	c.Now = func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }
	return c
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	cfg := filepath.Join(d, "shipit.yaml")
	if err := os.WriteFile(cfg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(d, "app")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return &rig{dir: proj, cfg: cfg, sys: sys{}, open: map[string]error{}}
}

func render(secs []Section) (string, int) {
	var b bytes.Buffer
	n := Print(&b, secs)
	return b.String(), n
}

const secret = "secret: 0123456789abcdef0123\n"

func goProject(dir string) string {
	return secret + "token: tok\nprojects:\n  api: {type: go, repo: o/api, dir: " + dir + ", service: api}\n"
}

func goSys(dir string) sys {
	return sys{
		showAPI: {out: "LoadState=loaded\nUser=shipit\nWorkingDirectory=" + dir + "/current\nEnvironmentFiles=/etc/api/.env (ignore_errors=no)"},
		"sudo -n -l /usr/bin/systemctl restart api": {out: "/usr/bin/systemctl restart api"},
	}
}

func TestAllGood(t *testing.T) {
	r := newRig(t)
	r.sys = goSys(r.dir)
	out, fails := render(newCheck(t, r, goProject(r.dir), false))
	if fails != 0 {
		t.Fatalf("unexpected failures:\n%s", out)
	}
	for _, want := range []string{"config", "webhook running on", "global token valid", "repo o/api reachable", "unit api.service found, User=shipit", "sudoers allows restart", "writable"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func newCheck(t *testing.T, r *rig, y string, offline bool) []Section {
	return r.checker(t, y, offline).All(context.Background())
}

func TestFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(r *rig)
		want   string
	}{
		{"unit missing", func(r *rig) {
			r.sys[showAPI] = struct {
				out string
				err error
			}{out: "LoadState=not-found\nUser=\nWorkingDirectory="}
		}, "not found"},
		{"unit runs as root", func(r *rig) {
			r.sys[showAPI] = struct {
				out string
				err error
			}{out: "LoadState=loaded\nUser=\nWorkingDirectory=" + r.dir + "/current"}
		}, "would run as root"},
		{"unit runs as root explicitly", func(r *rig) {
			r.sys[showAPI] = struct {
				out string
				err error
			}{out: "LoadState=loaded\nUser=root\nWorkingDirectory=" + r.dir + "/current"}
		}, "unit runs as root"},
		{"wrong working dir", func(r *rig) {
			r.sys[showAPI] = struct {
				out string
				err error
			}{out: "LoadState=loaded\nUser=shipit\nWorkingDirectory=" + r.dir}
		}, "expected " + "%DIR%/current"},
		{"sudoers missing", func(r *rig) {
			r.sys["sudo -n -l /usr/bin/systemctl restart api"] = struct {
				out string
				err error
			}{out: "Sorry, user shipit is not allowed to execute", err: errors.New("exit 1")}
		}, "sudoers does not allow"},
		{"repo unreachable", func(r *rig) { r.rem.repoErr = map[string]error{"o/api": errors.New("github: 404 Not Found")} }, "repo o/api: github: 404"},
		{"token invalid", func(r *rig) { r.rem.tokenErr = errors.New("github: 401 Unauthorized") }, "global token: github: 401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.sys = goSys(r.dir)
			tt.mutate(r)
			out, fails := render(newCheck(t, r, goProject(r.dir), false))
			if fails == 0 {
				t.Fatalf("expected a failure:\n%s", out)
			}
			want := strings.ReplaceAll(tt.want, "%DIR%", r.dir)
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		})
	}
}

func TestMissingAndReadOnlyDir(t *testing.T) {
	r := newRig(t)
	missing := filepath.Join(r.dir, "nope")
	out, fails := render(newCheck(t, r, secret+"projects:\n  web: {type: vite, repo: o/w, dir: "+missing+"}\n", true))
	if fails != 1 || !strings.Contains(out, "does not exist") {
		t.Errorf("missing dir:\n%s", out)
	}

	if os.Geteuid() != 0 {
		ro := filepath.Join(r.dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		out, fails = render(newCheck(t, r, secret+"projects:\n  web: {type: vite, repo: o/w, dir: "+ro+"}\n", true))
		if fails != 1 || !strings.Contains(out, "not writable") {
			t.Errorf("read-only dir:\n%s", out)
		}
	}
}

func TestViteSkipsServiceChecks(t *testing.T) {
	r := newRig(t) // r.sys is empty: any systemctl/sudo call would error out
	out, fails := render(newCheck(t, r, secret+"projects:\n  web: {type: vite, repo: o/w, dir: "+r.dir+"}\n", false))
	if fails != 0 {
		t.Errorf("vite project failed:\n%s", out)
	}
	if strings.Contains(out, "sudoers") || strings.Contains(out, "unit") {
		t.Errorf("vite project ran service checks:\n%s", out)
	}
}

func TestOfflineSkipsNetwork(t *testing.T) {
	r := newRig(t)
	r.sys = goSys(r.dir)
	r.rem = fakeRemote{tokenErr: errors.New("net down"), repoErr: map[string]error{"o/api": errors.New("net down")}}
	out, fails := render(newCheck(t, r, goProject(r.dir), true))
	if fails != 0 || strings.Contains(out, "net down") {
		t.Errorf("offline check touched the network:\n%s", out)
	}
}

func TestGlobalWarnings(t *testing.T) {
	r := newRig(t)
	out, fails := render(newCheck(t, r, secret, true))
	if fails != 0 || !strings.Contains(out, "no projects configured") {
		t.Errorf("empty config:\n%s", out)
	}

	out, _ = render(newCheck(t, r, secret+"projects:\n  web: {type: vite, repo: o/w, dir: "+r.dir+"}\n", true))
	if !strings.Contains(out, "no GitHub token") {
		t.Errorf("missing token warning:\n%s", out)
	}
	// A project-level token silences it.
	out, _ = render(newCheck(t, r, secret+"projects:\n  web: {type: vite, repo: o/w, dir: "+r.dir+", token: t}\n", true))
	if strings.Contains(out, "no GitHub token") {
		t.Errorf("warned although the project has a token:\n%s", out)
	}

	for _, tc := range []struct {
		name      string
		state     PortState
		listenErr error
		want      string
		fails     int
	}{
		{"shipit itself", PortShipit, nil, "webhook running on :7447 (shipit v1.2.0)", 0},
		{"other program", PortOther, nil, "does not identify as shipit", 1},
		{"nothing there", PortFree, nil, "webhook is not running", 0},
		{"cannot bind", PortFree, errors.New("permission denied"), "cannot listen on :7447: permission denied", 1},
	} {
		c := r.checker(t, secret, true)
		c.Probe = func(context.Context, string) (PortState, string) { return tc.state, "v1.2.0" }
		c.Listen = func(string) error { return tc.listenErr }
		out, fails = render(c.All(context.Background()))
		if fails != tc.fails || !strings.Contains(out, tc.want) {
			t.Errorf("%s: fails=%d, want %d, output:\n%s", tc.name, fails, tc.fails, out)
		}
	}
}

func TestConfigFileMode(t *testing.T) {
	r := newRig(t)
	if err := os.Chmod(r.cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	out, fails := render(newCheck(t, r, secret, true))
	if fails != 1 || !strings.Contains(out, "0644") {
		t.Errorf("mode check:\n%s", out)
	}
	out, fails = render(newCheck(t, r, "secret: short\n", true))
	if fails != 2 || !strings.Contains(out, "at least") {
		t.Errorf("secret check:\n%s", out)
	}
}

func TestTokenExpiry(t *testing.T) {
	for _, tc := range []struct{ expires, want string }{
		{"", "no expiry"},
		{"2026-10-10 00:00:00 UTC", "expires soon"},
		{"2027-03-01 00:00:00 UTC", "expires 2027-03-01"},
		{"someday", "expires someday"},
	} {
		r := newRig(t)
		r.rem.expires = tc.expires
		out, fails := render(newCheck(t, r, secret+"token: t\n", false))
		if fails != 0 || !strings.Contains(out, tc.want) {
			t.Errorf("expires %q: want %q in:\n%s", tc.expires, tc.want, out)
		}
	}
}

func unitOut(dir, user, env string) struct {
	out string
	err error
} {
	return struct {
		out string
		err error
	}{out: "LoadState=loaded\nUser=" + user + "\nWorkingDirectory=" + dir + "/current\nEnvironmentFiles=" + env}
}

func TestUnitAccounts(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		wantWarn bool
	}{
		{"own account", "svc-api", false},
		{"shared shipit account", "shipit", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.sys = goSys(r.dir)
			r.sys[showAPI] = unitOut(r.dir, tt.user, "/etc/api/.env (ignore_errors=no)")
			out, fails := render(newCheck(t, r, goProject(r.dir), false))
			if fails != 0 {
				t.Fatalf("unexpected failures:\n%s", out)
			}
			if !strings.Contains(out, "unit api.service found, User="+tt.user) {
				t.Errorf("unit not reported OK:\n%s", out)
			}
			if got := strings.Contains(out, "one compromised service can reach them all"); got != tt.wantWarn {
				t.Errorf("shared-account warning = %v, want %v:\n%s", got, tt.wantWarn, out)
			}
		})
	}
}

func TestEnvFiles(t *testing.T) {
	r0 := newRig(t)
	inProject := filepath.Join(r0.dir, "shared", ".env")

	tests := []struct {
		name    string
		env     string // value of EnvironmentFiles=
		open    map[string]error
		want    []string
		notWant []string
	}{
		{
			name:    "root-owned file is out of reach",
			env:     "/etc/api/.env (ignore_errors=no)",
			want:    []string{"✔ env file /etc/api/.env is not readable by shipit"},
			notWant: []string{"! env file"},
		},
		{
			name:    "readable file only warns",
			env:     "/etc/api/.env (ignore_errors=no)",
			open:    map[string]error{"/etc/api/.env": nil},
			want:    []string{"! env file /etc/api/.env can be read by shipit", "/etc/api/.env owned by root"},
			notWant: []string{"inside the project directory"},
		},
		{
			name: "readable file in the project directory",
			env:  inProject + " (ignore_errors=no)",
			open: map[string]error{inProject: nil},
			want: []string{"! env file " + inProject + " (inside the project directory) can be read by shipit"},
		},
		{
			name: "several files and one readable",
			env:  "/etc/api/.env (ignore_errors=no) /etc/api/extra.env (ignore_errors=yes)",
			open: map[string]error{"/etc/api/extra.env": nil},
			want: []string{"✔ env file /etc/api/.env is not readable", "! env file /etc/api/extra.env can be read"},
		},
		{
			name: "path with spaces",
			env:  "/etc/my api/.env (ignore_errors=no)",
			want: []string{"env file /etc/my api/.env is not readable"},
		},
		{
			name: "missing file the service needs",
			env:  "/etc/api/.env (ignore_errors=no)",
			open: map[string]error{"/etc/api/.env": fs.ErrNotExist},
			want: []string{"! env file /etc/api/.env does not exist"},
		},
		{
			name:    "missing optional file is fine",
			env:     "/etc/api/.env (ignore_errors=yes)",
			open:    map[string]error{"/etc/api/.env": fs.ErrNotExist},
			notWant: []string{"env file"},
		},
		{
			name: "unexpected error",
			env:  "/etc/api/.env (ignore_errors=no)",
			open: map[string]error{"/etc/api/.env": errors.New("input/output error")},
			want: []string{"! env file /etc/api/.env: input/output error"},
		},
		{
			name:    "no environment files",
			env:     "",
			notWant: []string{"env file"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.sys = goSys(r.dir)
			r.sys[showAPI] = unitOut(r.dir, "svc-api", strings.ReplaceAll(tt.env, inProject, filepath.Join(r.dir, "shared", ".env")))
			for k, v := range tt.open {
				r.open[strings.ReplaceAll(k, inProject, filepath.Join(r.dir, "shared", ".env"))] = v
			}
			out, fails := render(newCheck(t, r, goProject(r.dir), false))
			if fails != 0 {
				t.Fatalf("an env file problem must only warn, got %d failures:\n%s", fails, out)
			}
			out = strings.ReplaceAll(out, r.dir, "%DIR%")
			for _, w := range tt.want {
				if w = strings.ReplaceAll(w, inProject, "%DIR%/shared/.env"); !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestEnvFileGlob(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	one := filepath.Join(dir, "a.env")
	if err := os.WriteFile(one, []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.sys = goSys(r.dir)
	r.sys[showAPI] = unitOut(r.dir, "svc-api", filepath.Join(dir, "*.env")+" (ignore_errors=yes) "+filepath.Join(dir, "none-*.env")+" (ignore_errors=yes)")
	r.open[one] = nil
	out, fails := render(newCheck(t, r, goProject(r.dir), false))
	if fails != 0 || !strings.Contains(out, "! env file "+one+" can be read") {
		t.Errorf("glob not expanded:\n%s", out)
	}
	if strings.Contains(out, "none-") {
		t.Errorf("an unmatched glob must stay silent:\n%s", out)
	}
}

func TestCanOpen(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.env")
	if err := os.WriteFile(f, []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := canOpen(f); err != nil {
		t.Errorf("readable file: %v", err)
	}
	if err := canOpen(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: got %v, want ErrNotExist", err)
	}
	if os.Geteuid() != 0 { // root reads everything
		if err := os.Chmod(f, 0); err != nil {
			t.Fatal(err)
		}
		if err := canOpen(f); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("mode 0 file: got %v, want ErrPermission", err)
		}
	}
}

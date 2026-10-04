package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

type rig struct {
	dir string
	cfg string
	sys sys
	rem fakeRemote
}

func (r *rig) checker(t *testing.T, y string, offline bool) *Checker {
	t.Helper()
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, r.cfg, offline)
	c.Run = r.sys.run
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
	return &rig{dir: proj, cfg: cfg, sys: sys{}}
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
		"systemctl show api -p LoadState -p User -p WorkingDirectory": {out: "LoadState=loaded\nUser=shipit\nWorkingDirectory=" + dir + "/current"},
		"sudo -n -l /usr/bin/systemctl restart api":                   {out: "/usr/bin/systemctl restart api"},
	}
}

func TestAllGood(t *testing.T) {
	r := newRig(t)
	r.sys = goSys(r.dir)
	out, fails := render(newCheck(t, r, goProject(r.dir), false))
	if fails != 0 {
		t.Fatalf("unexpected failures:\n%s", out)
	}
	for _, want := range []string{"config", "port", "global token valid", "repo o/api reachable", "unit api.service found, User=shipit", "sudoers allows restart", "writable"} {
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
			r.sys["systemctl show api -p LoadState -p User -p WorkingDirectory"] = struct {
				out string
				err error
			}{out: "LoadState=not-found\nUser=\nWorkingDirectory="}
		}, "not found"},
		{"unit runs as root", func(r *rig) {
			r.sys["systemctl show api -p LoadState -p User -p WorkingDirectory"] = struct {
				out string
				err error
			}{out: "LoadState=loaded\nUser=\nWorkingDirectory=" + r.dir + "/current"}
		}, "would run as root"},
		{"unit other user", func(r *rig) {
			r.sys["systemctl show api -p LoadState -p User -p WorkingDirectory"] = struct {
				out string
				err error
			}{out: "LoadState=loaded\nUser=www-data\nWorkingDirectory=" + r.dir + "/current"}
		}, `runs as "www-data"`},
		{"wrong working dir", func(r *rig) {
			r.sys["systemctl show api -p LoadState -p User -p WorkingDirectory"] = struct {
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

	c := r.checker(t, secret, true)
	c.Listen = func(string) error { return errors.New("address already in use") }
	out, fails = render(c.All(context.Background()))
	if fails != 0 || !strings.Contains(out, "already in use") {
		t.Errorf("listen conflict should only warn:\n%s", out)
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

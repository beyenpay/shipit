package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beyenpay/shipit/internal/release"
)

type fakeSource struct {
	latest   string
	tags     map[string]bool // releases that exist
	payload  string
	dlErr    error
	repoUsed string
	asked    []string
	assets   []string
}

func (f *fakeSource) Release(_ context.Context, repo, tag string) (*release.Release, error) {
	f.repoUsed = repo
	if tag == "" {
		tag = f.latest
	}
	if !f.tags[tag] {
		return nil, errors.New("github: 404 Not Found")
	}
	return &release.Release{Tag: tag}, nil
}

func (f *fakeSource) DownloadVerified(_ context.Context, _ string, rel *release.Release, name, dst string) error {
	f.assets = append(f.assets, name)
	f.asked = append(f.asked, rel.Tag)
	if f.dlErr != nil {
		return f.dlErr
	}
	return os.WriteFile(dst, []byte(f.payload), 0o600)
}

type rig struct {
	t       *testing.T
	dir     string
	bin     string
	unit    string
	out     bytes.Buffer
	cmds    []string
	running bool // is the service up right now
	// crashes: after a restart the service is down while the new binary is
	// installed (it starts, then dies).
	crashes bool
	// restartErr: `systemctl restart` itself fails while the new binary is
	// installed.
	restartErr bool
	probeOut   string
	probeErr   error
	src        *fakeSource
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir, bin: filepath.Join(dir, "shipit"), unit: filepath.Join(dir, "shipit.service"), running: true}
	r.write(r.bin, "OLD")
	r.write(r.unit, "[Unit]")
	r.src = &fakeSource{latest: "v1.1.0", tags: map[string]bool{"v1.0.0": true, "v1.1.0": true, "v0.9.0": true}, payload: "NEW"}
	r.probeOut = "v1.1.0"
	return r
}

func (r *rig) write(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (r *rig) opts(version, target string) Options {
	return Options{
		Version: version,
		Target:  target,
		Binary:  r.bin,
		Unit:    r.unit,
		Arch:    "amd64",
		IsRoot:  true,
		Out:     &r.out,
		Source:  r.src,
		Run: func(name string, args ...string) error {
			cmd := name + " " + strings.Join(args, " ")
			r.cmds = append(r.cmds, cmd)
			switch {
			case strings.Contains(cmd, "is-active"):
				if !r.running {
					return errors.New("inactive")
				}
			case strings.Contains(cmd, "restart"):
				isNew := r.read(r.bin) == "NEW"
				if r.restartErr && isNew {
					return errors.New("restart failed")
				}
				r.running = !(r.crashes && isNew)
			}
			return nil
		},
		Probe: func(path string) (string, error) {
			if r.read(path) != r.src.payload {
				return "", errors.New("unexpected file")
			}
			return r.probeOut, r.probeErr
		},
	}
}

func TestUpdateToLatest(t *testing.T) {
	r := newRig(t)
	if err := Run(context.Background(), r.opts("v1.0.0", "")); err != nil {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
	if r.read(r.bin) != "NEW" {
		t.Error("binary not replaced")
	}
	if r.read(r.bin+".old") != "OLD" {
		t.Error("previous binary not kept")
	}
	if _, err := os.Stat(r.bin + ".new"); err == nil {
		t.Error("temporary file left behind")
	}
	fi, _ := os.Stat(r.bin)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if r.src.repoUsed != "beyenpay/shipit" || len(r.src.assets) != 1 || r.src.assets[0] != "shipit-linux-amd64" {
		t.Errorf("repo=%q assets=%v", r.src.repoUsed, r.src.assets)
	}
	want := "systemctl is-active --quiet shipit.service|systemctl restart shipit.service|systemctl is-active --quiet shipit.service"
	if got := strings.Join(r.cmds, "|"); got != want {
		t.Errorf("commands = %q", got)
	}
	if !strings.Contains(r.out.String(), "now on v1.1.0") {
		t.Errorf("output:\n%s", r.out.String())
	}
}

func TestSpecificVersionAndDowngrade(t *testing.T) {
	r := newRig(t)
	r.probeOut = "v0.9.0"
	if err := Run(context.Background(), r.opts("v1.1.0", "v0.9.0")); err != nil {
		t.Fatal(err)
	}
	if r.src.asked[0] != "v0.9.0" || r.read(r.bin) != "NEW" {
		t.Errorf("asked=%v", r.src.asked)
	}
}

func TestArm64Asset(t *testing.T) {
	r := newRig(t)
	o := r.opts("v1.0.0", "")
	o.Arch = "arm64"
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.src.assets[0] != "shipit-linux-arm64" {
		t.Errorf("assets = %v", r.src.assets)
	}
}

func TestAlreadyUpToDate(t *testing.T) {
	r := newRig(t)
	if err := Run(context.Background(), r.opts("v1.1.0", "")); err != nil {
		t.Fatal(err)
	}
	if len(r.src.assets) != 0 || len(r.cmds) != 0 || r.read(r.bin) != "OLD" {
		t.Error("an up-to-date install was touched")
	}
	if !strings.Contains(r.out.String(), "already on v1.1.0") {
		t.Errorf("output:\n%s", r.out.String())
	}
}

func TestDryRun(t *testing.T) {
	r := newRig(t)
	o := r.opts("v1.0.0", "")
	o.DryRun, o.IsRoot = true, false
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(r.src.assets) != 0 || len(r.cmds) != 0 || r.read(r.bin) != "OLD" {
		t.Error("dry run changed something")
	}
	if !strings.Contains(r.out.String(), "v1.0.0 -> v1.1.0") {
		t.Errorf("output:\n%s", r.out.String())
	}
}

func TestNeedsRoot(t *testing.T) {
	r := newRig(t)
	o := r.opts("v1.0.0", "")
	o.IsRoot = false
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("err = %v", err)
	}
	if len(r.src.assets) != 0 {
		t.Error("downloaded without root")
	}
}

func TestInvalidAndUnknownVersion(t *testing.T) {
	r := newRig(t)
	for _, v := range []string{"../x", "a/b", ".hidden"} {
		if err := Run(context.Background(), r.opts("v1.0.0", v)); err == nil || !strings.Contains(err.Error(), "invalid version") {
			t.Errorf("%q: err = %v", v, err)
		}
	}
	if err := Run(context.Background(), r.opts("v1.0.0", "v9.9.9")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown version: %v", err)
	}
	if r.read(r.bin) != "OLD" {
		t.Error("binary changed")
	}
}

func TestDownloadFailureChangesNothing(t *testing.T) {
	r := newRig(t)
	r.src.dlErr = errors.New("sha256 mismatch")
	err := Run(context.Background(), r.opts("v1.0.0", ""))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if r.read(r.bin) != "OLD" || len(r.cmds) != 0 {
		t.Error("state changed after a failed download")
	}
	if _, err := os.Stat(r.bin + ".new"); err == nil {
		t.Error("temporary file left behind")
	}
}

func TestBrokenNewBinaryIsNeverInstalled(t *testing.T) {
	r := newRig(t)
	r.probeErr = errors.New("exec format error")
	err := Run(context.Background(), r.opts("v1.0.0", ""))
	if err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("err = %v", err)
	}
	if r.read(r.bin) != "OLD" || len(r.cmds) != 0 {
		t.Error("a binary that cannot run was installed")
	}
	if _, err := os.Stat(r.bin + ".new"); err == nil {
		t.Error("temporary file left behind")
	}
}

func TestWrongVersionBinaryIsRejected(t *testing.T) {
	r := newRig(t)
	r.probeOut = "v1.0.0" // the release asset is not what it claims to be
	err := Run(context.Background(), r.opts("v0.8.0", ""))
	if err == nil || !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("err = %v", err)
	}
	if r.read(r.bin) != "OLD" {
		t.Error("binary replaced")
	}
}

func TestFailedUpdateRestoresOldBinary(t *testing.T) {
	for name, set := range map[string]func(*rig){
		"service dies after restart": func(r *rig) { r.crashes = true },
		"restart command fails":      func(r *rig) { r.restartErr = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			set(r)
			err := Run(context.Background(), r.opts("v1.0.0", ""))
			if err == nil || !strings.Contains(err.Error(), "previous version was restored") {
				t.Fatalf("err = %v\n%s", err, r.out.String())
			}
			if r.read(r.bin) != "OLD" {
				t.Errorf("binary = %q, want the old one back", r.read(r.bin))
			}
			if last := r.cmds[len(r.cmds)-1]; last != "systemctl restart shipit.service" {
				t.Errorf("old version was not restarted, commands = %v", r.cmds)
			}
			if !r.running {
				t.Error("service left down")
			}
		})
	}
}

func TestServiceNotRunningIsNotStarted(t *testing.T) {
	r := newRig(t)
	r.running = false
	if err := Run(context.Background(), r.opts("v1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if r.read(r.bin) != "NEW" {
		t.Error("binary not replaced")
	}
	for _, c := range r.cmds {
		if strings.Contains(c, "restart") {
			t.Errorf("restarted a service that was not running: %v", r.cmds)
		}
	}
	if !strings.Contains(r.out.String(), "not running") {
		t.Errorf("output:\n%s", r.out.String())
	}
}

func TestNoUnitMeansNoRestart(t *testing.T) {
	r := newRig(t)
	os.Remove(r.unit)
	if err := Run(context.Background(), r.opts("v1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if len(r.cmds) != 0 {
		t.Errorf("commands = %v", r.cmds)
	}
}

func TestNotInstalled(t *testing.T) {
	r := newRig(t)
	os.Remove(r.bin)
	err := Run(context.Background(), r.opts("v1.0.0", ""))
	if err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Errorf("err = %v", err)
	}
	if len(r.src.assets) != 0 {
		t.Error("downloaded although nothing is installed")
	}
}

func TestReplacesRunningBinaryAtomically(t *testing.T) {
	// A hard link must keep the old content readable while the path points at
	// the new one, i.e. the swap is a rename, not an in-place rewrite.
	r := newRig(t)
	if err := Run(context.Background(), r.opts("v1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(r.bin)
	b, _ := os.Stat(r.bin + ".old")
	if os.SameFile(a, b) {
		t.Error("new and old binary are the same file")
	}
}

func TestParseVersionOutput(t *testing.T) {
	for in, want := range map[string]string{"shipit v1.2.3\n": "v1.2.3", "shipit dev": "dev", "v1.0.0": "v1.0.0", "  shipit   v2 \n": "v2"} {
		if got := ParseVersionOutput(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

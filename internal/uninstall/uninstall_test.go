package uninstall

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beyenpay/shipit/internal/config"
)

type world struct {
	t     *testing.T
	root  string
	paths Paths
	cmds  []string
	out   bytes.Buffer
	proj  string // a real-looking project dir
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, root: root, paths: Paths{
		Binary:    filepath.Join(root, "bin", "shipit"),
		ConfigDir: filepath.Join(root, "etc", "shipit"),
		Unit:      filepath.Join(root, "systemd", "shipit.service"),
	}, proj: filepath.Join(root, "srv", "app", "web")}
	for path, content := range map[string]string{
		w.paths.Binary: "bin",
		filepath.Join(w.paths.ConfigDir, "shipit.yaml"): "secret",
		w.paths.Unit: "[Unit]",
		filepath.Join(w.proj, "releases", "v1", "index.html"): "<html>",
		filepath.Join(w.proj, "shared", ".env"):               "A=1",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *world) opts(in string) Options {
	return Options{
		Paths:  w.paths,
		In:     strings.NewReader(in),
		Out:    &w.out,
		IsRoot: true,
		Projects: []*config.Project{
			{Name: "web", Dir: w.proj, Service: "web"},
		},
		Run: func(name string, args ...string) error {
			w.cmds = append(w.cmds, name+" "+strings.Join(args, " "))
			return nil
		},
	}
}

func (w *world) has(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestPlainUninstallKeepsConfigAndProjects(t *testing.T) {
	w := newWorld(t)
	if err := Run(w.opts("yes\n")); err != nil {
		t.Fatalf("%v\n%s", err, w.out.String())
	}
	if w.has(w.paths.Binary) || w.has(w.paths.Unit) {
		t.Error("binary or unit left behind")
	}
	if !w.has(w.paths.ConfigDir) || !w.has(w.proj) {
		t.Error("plain uninstall removed config or project data")
	}
	want := "systemctl disable --now shipit.service|systemctl daemon-reload"
	if got := strings.Join(w.cmds, "|"); got != want {
		t.Errorf("commands = %q, want %q", got, want)
	}
	out := w.out.String()
	for _, s := range []string{"/etc/sudoers.d/shipit", "/etc/systemd/system/web.service", w.proj} {
		if !strings.Contains(out, s) {
			t.Errorf("output does not mention %q:\n%s", s, out)
		}
	}
}

func TestAbortChangesNothing(t *testing.T) {
	for _, in := range []string{"no\n", "\n", "", "y\n"} {
		w := newWorld(t)
		err := Run(w.opts(in))
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Errorf("input %q: err = %v", in, err)
		}
		if !w.has(w.paths.Binary) || !w.has(w.paths.Unit) || len(w.cmds) != 0 {
			t.Errorf("input %q: something was changed", in)
		}
	}
}

func TestPurgeRemovesConfigButNotProjects(t *testing.T) {
	w := newWorld(t)
	// "yes" to continue, "n" to deleting the user
	if err := Run(withPurge(w.opts("yes\nn\n"))); err != nil {
		t.Fatalf("%v\n%s", err, w.out.String())
	}
	if w.has(w.paths.ConfigDir) {
		t.Error("config dir survived --purge")
	}
	if !w.has(w.proj) {
		t.Error("--purge deleted project data")
	}
	for _, c := range w.cmds {
		if strings.HasPrefix(c, "userdel") {
			t.Error("user deleted although the answer was no")
		}
	}
}

func withPurge(o Options) Options { o.Purge = true; return o }

func TestPurgeDeletesUserOnRequest(t *testing.T) {
	w := newWorld(t)
	if err := Run(withPurge(w.opts("yes\ny\n"))); err != nil {
		t.Fatal(err)
	}
	last := w.cmds[len(w.cmds)-1]
	if last != "userdel --remove shipit" {
		t.Errorf("commands = %v", w.cmds)
	}
	if w.has(w.paths.Binary) {
		t.Error("binary left behind")
	}
}

func TestYesNeverDeletesUser(t *testing.T) {
	w := newWorld(t)
	o := withPurge(w.opts(""))
	o.Yes = true
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.cmds {
		if strings.HasPrefix(c, "userdel") {
			t.Error("-y must not delete the user")
		}
	}
}

func TestDeleteProjectsNeedsSecondConfirmation(t *testing.T) {
	w := newWorld(t)
	o := withPurge(w.opts("yes\nnope\n"))
	o.DeleteProjects = true
	err := Run(o)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err = %v", err)
	}
	if !w.has(w.proj) || !w.has(w.paths.Binary) {
		t.Error("declined second confirmation but data was removed")
	}

	w = newWorld(t)
	o = withPurge(w.opts("yes\ndelete\nn\n"))
	o.DeleteProjects = true
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s", err, w.out.String())
	}
	if w.has(w.proj) {
		t.Error("project directory survived --delete-projects")
	}
	if strings.Contains(w.out.String(), "(project files)") {
		t.Error("told the user to review project files that were deleted")
	}
}

func TestDeleteProjectsRequiresPurge(t *testing.T) {
	w := newWorld(t)
	o := w.opts("yes\n")
	o.DeleteProjects = true
	if err := Run(o); err == nil || !strings.Contains(err.Error(), "--purge") {
		t.Errorf("err = %v", err)
	}
}

func TestDryRun(t *testing.T) {
	w := newWorld(t)
	o := withPurge(w.opts(""))
	o.DeleteProjects, o.DryRun = true, true
	o.IsRoot = false // dry runs do not need root
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	if !w.has(w.paths.Binary) || !w.has(w.paths.ConfigDir) || !w.has(w.proj) || len(w.cmds) != 0 {
		t.Error("dry run changed something")
	}
	if !strings.Contains(w.out.String(), "DELETE project directory") {
		t.Errorf("plan not shown:\n%s", w.out.String())
	}
}

func TestNeedsRoot(t *testing.T) {
	w := newWorld(t)
	o := w.opts("yes\n")
	o.IsRoot = false
	if err := Run(o); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("err = %v", err)
	}
}

func TestNothingToUninstall(t *testing.T) {
	w := newWorld(t)
	os.Remove(w.paths.Binary)
	os.Remove(w.paths.Unit)
	if err := Run(w.opts("yes\n")); err == nil || !strings.Contains(err.Error(), "nothing to uninstall") {
		t.Errorf("err = %v", err)
	}
}

func TestFailuresAreReportedButDoNotStopTheRest(t *testing.T) {
	w := newWorld(t)
	o := w.opts("yes\n")
	o.Run = func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "disable" {
			return errors.New("boom")
		}
		return nil
	}
	err := Run(o)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if w.has(w.paths.Binary) {
		t.Error("later steps did not run after a failure")
	}
}

func TestSafeProjectDir(t *testing.T) {
	w := newWorld(t)
	link := filepath.Join(w.root, "srv", "link")
	if err := os.Symlink(w.proj, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(w.root, "srv", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		dir, want string
	}{
		{w.proj, ""},
		{"/", "too close"},
		{"/srv", "too close"},
		{"/etc", "too close"},
		{"/var/lib", "shared system"},
		{"/var/www", "shared system"},
		{filepath.Join(w.root, "nope"), "does not exist"},
		{link, "not a real directory"},
		{plain, "no releases/"},
		{"relative/dir", "too close"},
	}
	for _, tt := range tests {
		err := safeProjectDir(tt.dir)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.dir, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want %q", tt.dir, err, tt.want)
		}
	}
}

func TestUnsafeProjectDirIsSkipped(t *testing.T) {
	w := newWorld(t)
	plain := filepath.Join(w.root, "srv", "plain")
	os.MkdirAll(plain, 0o755)
	os.WriteFile(filepath.Join(plain, "important"), []byte("x"), 0o644)

	o := withPurge(w.opts(""))
	o.Yes, o.DeleteProjects = true, true
	o.Projects = []*config.Project{{Name: "p", Dir: plain}}
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	if !w.has(filepath.Join(plain, "important")) {
		t.Error("deleted a directory that does not look like a shipit project")
	}
	if !strings.Contains(w.out.String(), "not deleting") {
		t.Errorf("no explanation shown:\n%s", w.out.String())
	}
}

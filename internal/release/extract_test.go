package release

import (
	"archive/tar"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type ent struct {
	name string
	typ  byte
	body string
	link string
	mode int64
}

func mktar(t *testing.T, ents []ent) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractOK(t *testing.T) {
	arc := mktar(t, []ent{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./app", body: "bin", mode: 0o775},
		{name: "static/", typ: tar.TypeDir, mode: 0o755},
		{name: "static/a.txt", body: "hello"},
		{name: "latest", typ: tar.TypeSymlink, link: "static/a.txt"},
		{name: "static/up", typ: tar.TypeSymlink, link: "../app"},
		{name: "deep/er/file", body: "x"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(arc, dest); err != nil {
		t.Fatal(err)
	}

	if b, _ := os.ReadFile(filepath.Join(dest, "static/a.txt")); string(b) != "hello" {
		t.Errorf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "latest")); string(b) != "hello" {
		t.Errorf("symlink read = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "static/up")); string(b) != "bin" {
		t.Errorf("symlink up read = %q", b)
	}
	fi, err := os.Stat(filepath.Join(dest, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("exec bit lost: %v", fi.Mode())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("group/other write bit kept: %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dest, "deep/er/file")); err != nil {
		t.Error(err)
	}
}

func TestExtractRejects(t *testing.T) {
	tests := []struct {
		name string
		ents []ent
	}{
		{"parent dir", []ent{{name: "../evil", body: "x"}}},
		{"nested parent dir", []ent{{name: "a/../../evil", body: "x"}}},
		{"absolute", []ent{{name: "/tmp/evil", body: "x"}}},
		{"symlink absolute", []ent{{name: "l", typ: tar.TypeSymlink, link: "/etc"}}},
		{"symlink escapes", []ent{{name: "l", typ: tar.TypeSymlink, link: "../x"}}},
		{"symlink too many ups", []ent{{name: "d/l", typ: tar.TypeSymlink, link: "../../x"}}},
		{"symlink dotdot after name", []ent{{name: "l", typ: tar.TypeSymlink, link: "p/../.."}}},
		{"symlink empty", []ent{{name: "l", typ: tar.TypeSymlink, link: ""}}},
		{"hardlink", []ent{{name: "f", body: "x"}, {name: "h", typ: tar.TypeLink, link: "f"}}},
		{"fifo", []ent{{name: "p", typ: tar.TypeFifo}}},
		{"write through symlink", []ent{
			{name: "sub/", typ: tar.TypeDir},
			{name: "a", typ: tar.TypeSymlink, link: "sub"},
			{name: "a/x", body: "x"},
		}},
		{"duplicate file", []ent{{name: "f", body: "1"}, {name: "f", body: "2"}}},
		{"dir over file", []ent{{name: "f", body: "1"}, {name: "f/", typ: tar.TypeDir}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "out")
			if err := Extract(mktar(t, tt.ents), dest); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Lstat(filepath.Join(root, "evil")); err == nil {
				t.Error("file escaped dest")
			}
		})
	}
}

func TestExtractLimits(t *testing.T) {
	dest := func() string { return filepath.Join(t.TempDir(), "out") }

	big := mktar(t, []ent{{name: "f", body: strings.Repeat("x", 10)}})
	if err := extract(big, dest(), 5, 100); err == nil {
		t.Error("size limit not enforced")
	}
	many := mktar(t, []ent{{name: "a"}, {name: "b"}, {name: "c"}})
	if err := extract(many, dest(), 100, 2); err == nil {
		t.Error("entry limit not enforced")
	}
	if err := extract(many, dest(), 100, 3); err != nil {
		t.Error(err)
	}
}

func TestExtractNotGzip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(p, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Extract(p, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Error("expected error")
	}
}

func TestGroupAccess(t *testing.T) {
	tests := []struct{ in, want fs.FileMode }{
		{0o700, 0o750},
		{0o600, 0o640},
		{0o500, 0o550},
		{0o400, 0o440},
		{0o755, 0o755},
		{0o644, 0o644},
		{0o705, 0o755}, // other bits are left alone
		{0o200, 0o200}, // nothing to share
		{0o000, 0o000},
	}
	for _, tt := range tests {
		if got := groupAccess(tt.in); got != tt.want {
			t.Errorf("groupAccess(%04o) = %04o, want %04o", tt.in, got, tt.want)
		}
	}
}

// setUmask changes the umask for one test.
func setUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestExtractGroupCanReadAndRun(t *testing.T) {
	// A strict umask must not take the group's access away, and an archive
	// built with private modes must still be usable by the service account.
	setUmask(t, 0o077)
	arc := mktar(t, []ent{
		{name: "./", typ: tar.TypeDir, mode: 0o700},
		{name: "app", body: "bin", mode: 0o700},
		{name: "secret.txt", body: "x", mode: 0o600},
		{name: "private/", typ: tar.TypeDir, mode: 0o700},
		{name: "private/data", body: "y", mode: 0o600},
		{name: "implicit/dir/file", body: "z", mode: 0o644},
	})
	dest := filepath.Join(t.TempDir(), "rel", "out")
	if err := Extract(arc, dest); err != nil {
		t.Fatal(err)
	}
	want := map[string]fs.FileMode{
		"":                  0o755, // dest and its created parents
		"app":               0o750,
		"secret.txt":        0o640,
		"private":           0o750,
		"private/data":      0o640,
		"implicit":          0o755,
		"implicit/dir":      0o755,
		"implicit/dir/file": 0o644,
	}
	for rel, mode := range want {
		fi, err := os.Stat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%q has mode %04o, want %04o", rel, got, mode)
		}
	}
	// The parent that Extract had to create must be reachable as well.
	if fi, err := os.Stat(filepath.Dir(dest)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("created parent of dest: %v %v", fi, err)
	}
}

func TestMkdirAllKeepsExistingAndSetgid(t *testing.T) {
	setUmask(t, 0o077)
	base := t.TempDir()
	if err := os.Chmod(base, 0o750|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	// An existing directory is never touched.
	if err := MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(base); fi.Mode().Perm() != 0o750 || fi.Mode()&fs.ModeSetgid == 0 {
		t.Errorf("existing directory changed: %v", fi.Mode())
	}

	// A directory that inherits setgid keeps it; where the OS does not
	// inherit it, there is nothing to keep.
	probe := filepath.Join(base, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	inherits := func() bool { fi, _ := os.Stat(probe); return fi.Mode()&fs.ModeSetgid != 0 }()

	deep := filepath.Join(base, "a", "b")
	if err := MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(base, "a"), deep} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %04o, want 0755", d, fi.Mode().Perm())
		}
		if inherits && fi.Mode()&fs.ModeSetgid == 0 {
			t.Errorf("%s lost the setgid bit", d)
		}
	}
}

func TestMkdirAllErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(filepath.Join(f, "sub"), 0o755); err == nil {
		t.Error("creating a directory below a regular file must fail")
	}
}

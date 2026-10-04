package release

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
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

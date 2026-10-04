package deploy

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckELF(t *testing.T) {
	write := func(name string, b []byte) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	be := []byte(elfBin("amd64"))
	be[5] = 2 // big endian
	binary.BigEndian.PutUint16(be[18:], 62)
	badData := []byte(elfBin("amd64"))
	badData[5] = 9

	tests := []struct {
		name string
		file []byte
		arch string
		want string // substring of the error, "" for success
	}{
		{"amd64 ok", []byte(elfBin("amd64")), "amd64", ""},
		{"arm64 ok", []byte(elfBin("arm64")), "arm64", ""},
		{"big endian ok", be, "amd64", ""},
		{"wrong arch", []byte(elfBin("arm64")), "amd64", "EM_AARCH64"},
		{"wrong arch other way", []byte(elfBin("amd64")), "arm64", "EM_X86_64"},
		{"unsupported server arch", []byte(elfBin("amd64")), "riscv64", "unsupported server architecture"},
		{"not elf", []byte("#!/bin/sh\necho hello world, definitely not elf\n"), "amd64", "not an ELF"},
		{"too short", []byte("\x7fELF"), "amd64", "not an ELF"},
		{"bad data encoding", badData, "amd64", "invalid ELF header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkELF(write("bin", tt.file), "bin", tt.arch)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantErr(t, err, tt.want)
		})
	}
}

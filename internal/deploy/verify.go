package deploy

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/beyenpay/shipit/internal/config"
)

var elfMachine = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// verifyRelease checks that dir looks like a complete release for the
// project's type. It runs before anything goes live, so a wrongly packaged
// release is rejected while the old version is still serving.
func verifyRelease(p *config.Project, dir, arch string) error {
	switch p.Type {
	case config.TypeGo:
		bin := filepath.Join(dir, p.Asset)
		if err := requireFile(bin, p.Asset); err != nil {
			return err
		}
		if fi, err := os.Stat(bin); err != nil {
			return err
		} else if fi.Mode().Perm()&0o100 == 0 {
			return fmt.Errorf("%s is not executable", p.Asset)
		}
		return checkELF(bin, p.Asset, arch)
	case config.TypeNext:
		if err := requireFile(filepath.Join(dir, "server.js"), "server.js"); err != nil {
			return err
		}
		fi, err := os.Stat(filepath.Join(dir, ".next", "static"))
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("missing .next/static (the package must include it next to the standalone output)")
		}
		return nil
	case config.TypeVite:
		return requireFile(filepath.Join(dir, "index.html"), "index.html")
	}
	return fmt.Errorf("unknown project type %q", p.Type)
}

func requireFile(path, name string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("missing %s in the release", name)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s in the release is not a regular file", name)
	}
	return nil
}

// checkELF reads just the ELF header and compares the CPU architecture with
// the one this server runs on.
func checkELF(path, name, arch string) error {
	want, ok := elfMachine[arch]
	if !ok {
		return fmt.Errorf("unsupported server architecture %q", arch)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var h [20]byte
	if _, err := io.ReadFull(f, h[:]); err != nil || string(h[:4]) != elf.ELFMAG {
		return fmt.Errorf("%s is not an ELF binary (build with GOOS=linux)", name)
	}
	var bo binary.ByteOrder
	switch elf.Data(h[5]) {
	case elf.ELFDATA2LSB:
		bo = binary.LittleEndian
	case elf.ELFDATA2MSB:
		bo = binary.BigEndian
	default:
		return fmt.Errorf("%s has an invalid ELF header", name)
	}
	if got := elf.Machine(bo.Uint16(h[18:20])); got != want {
		return fmt.Errorf("%s is built for %s but this server is %s", name, got, arch)
	}
	return nil
}

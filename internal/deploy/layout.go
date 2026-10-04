package deploy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/beyenpay/shipit/internal/config"
)

// On-disk layout of a project directory:
//
//	releases/<tag>/   one directory per installed release
//	current           symlink -> releases/<tag>   (what is live)
//	previous          symlink -> releases/<tag>   (what rollback returns to)
//	.shipit.lock      flock(2) lock file
//
// The two symlinks are the only state shipit keeps. Everything else is
// derived from the directory contents.
const (
	releasesName = "releases"
	currentName  = "current"
	previousName = "previous"
	lockName     = ".shipit.lock"
)

// ErrBusy is returned when another deploy or rollback holds the project lock.
var ErrBusy = errors.New("another operation is in progress for this project")

func releasesDir(p *config.Project) string { return filepath.Join(p.Dir, releasesName) }

func releaseDir(p *config.Project, tag string) string {
	return filepath.Join(releasesDir(p), tag)
}

func linkPath(p *config.Project, name string) string { return filepath.Join(p.Dir, name) }

func hasRelease(p *config.Project, tag string) bool {
	fi, err := os.Stat(releaseDir(p, tag))
	return err == nil && fi.IsDir()
}

// readTag returns the tag the named link points to, or "" if the link does not
// exist. Anything that is not a symlink into releases/ is an error, so that we
// never overwrite something we did not create (for example a directory named
// "current" left over from a manual deployment).
func readTag(p *config.Project, name string) (string, error) {
	link := linkPath(p, name)
	fi, err := os.Lstat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s exists but is not a symlink; move it away first", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	tag, ok := strings.CutPrefix(target, releasesName+"/")
	if !ok || !config.ValidTag(tag) {
		return "", fmt.Errorf("%s points to %q, which is not a shipit release", link, target)
	}
	return tag, nil
}

// setLink atomically points the named link at releases/<tag>: it creates a
// temporary symlink and renames it over the old one.
func setLink(p *config.Project, name, tag string) error {
	link := linkPath(p, name)
	tmp := filepath.Join(p.Dir, "."+name+".tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(releasesName+"/"+tag, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func removeLink(p *config.Project, name string) error {
	err := os.Remove(linkPath(p, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type releaseInfo struct {
	Tag string
	Mod time.Time
}

// listReleases returns installed releases, most recently used first.
// Dot-prefixed entries (downloads in progress) are ignored.
func listReleases(p *config.Project) ([]releaseInfo, error) {
	entries, err := os.ReadDir(releasesDir(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []releaseInfo
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, releaseInfo{Tag: e.Name(), Mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Mod.Equal(out[j].Mod) {
			return out[i].Mod.After(out[j].Mod)
		}
		return out[i].Tag > out[j].Tag
	})
	return out, nil
}

// lock is an exclusive flock(2) on the project's lock file. The kernel drops
// it when the process dies, so a crash can never leave a stale lock behind.
type lock struct{ f *os.File }

func acquire(path string) (*lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &lock{f: f}, nil
}

func (l *lock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

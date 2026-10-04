package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxExtractBytes   = 4 << 30 // total unpacked size, guards against decompression bombs
	maxExtractEntries = 200000
)

// Extract unpacks the .tar.gz at archive into dest (created if missing).
//
// It is strict about what it accepts: only directories, regular files and
// relative symlinks that stay inside dest. Absolute paths, "..", hard links,
// devices and writes through symlinks are errors. Files are never overwritten,
// and group/other write bits are dropped. The archive is unpacked as-is: a
// leading top-level directory is not stripped.
func Extract(archive, dest string) error {
	return extract(archive, dest, maxExtractBytes, maxExtractEntries)
}

func extract(archive, dest string, maxBytes int64, maxEntries int) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	remaining := maxBytes
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if n >= maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		rel, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" { // the archive root, e.g. "./"
			continue
		}
		if err := noSymlinkParents(dest, rel); err != nil {
			return err
		}
		target := filepath.Join(dest, rel)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := makeDir(target, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > remaining {
				return fmt.Errorf("archive unpacks to more than %d bytes", maxBytes)
			}
			remaining -= hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, hdr.FileInfo().Mode().Perm()&^0o022); err != nil {
				return fmt.Errorf("extract %s: %w", rel, err)
			}
		case tar.TypeSymlink:
			if err := checkLink(rel, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("extract %s: %w", rel, err)
			}
		default:
			return fmt.Errorf("unsupported entry %q (type %q)", hdr.Name, hdr.Typeflag)
		}
	}
}

func cleanName(name string) (string, error) {
	if strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return clean, nil
}

// noSymlinkParents makes sure no existing parent directory of rel is a
// symlink, so a file can never be written through a link planted earlier in
// the same archive.
func noSymlinkParents(dest, rel string) error {
	dir := dest
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // we create the rest ourselves, as real directories
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("unsafe path in archive: %q has a non-directory parent", rel)
		}
	}
	return nil
}

// checkLink accepts a symlink only if its target is relative and written as
// zero or more leading ".." followed by plain names, never climbing higher than
// dest. Together with noSymlinkParents (the link's own directory is always
// real) this guarantees that following the link, or any chain of such links,
// stays inside dest. A ".." after a name is refused because it could be
// resolved through another symlink.
func checkLink(rel, target string) error {
	bad := fmt.Errorf("unsafe symlink %s -> %q", rel, target)
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return bad
	}
	depth := strings.Count(rel, "/") // number of directories above the link
	ups, named := 0, false
	for p := range strings.SplitSeq(target, "/") {
		switch p {
		case "", ".":
		case "..":
			if named {
				return bad
			}
			ups++
		default:
			named = true
		}
	}
	if ups > depth {
		return bad
	}
	return nil
}

func makeDir(target string, perm fs.FileMode) error {
	fi, err := os.Lstat(target)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("archive entry %s conflicts with an existing non-directory", target)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		// Keep owner rwx so we can fill it, whatever the archive says.
		return os.MkdirAll(target, (perm&^0o022)|0o700)
	default:
		return err
	}
}

func writeFile(target string, r io.Reader, perm fs.FileMode) error {
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

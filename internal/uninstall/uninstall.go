// Package uninstall removes shipit from a machine, in two modes: just the
// tool, or the tool plus its configuration (and optionally project data).
package uninstall

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/beyenpay/shipit/internal/config"
)

// Paths are the locations install.sh creates. They are fields only so tests
// can point them at a temporary directory.
type Paths struct {
	Binary    string
	ConfigDir string
	Unit      string
}

var DefaultPaths = Paths{
	Binary:    "/usr/local/bin/shipit",
	ConfigDir: "/etc/shipit",
	Unit:      "/etc/systemd/system/shipit.service",
}

const (
	UnitName = "shipit.service"
	UserName = "shipit"
)

type Options struct {
	// Purge also removes the configuration directory (secret and tokens).
	Purge bool
	// DeleteProjects also deletes the working directories of the projects in
	// Projects: releases, shared files, everything.
	DeleteProjects bool
	// Yes skips the confirmation questions.
	Yes bool
	// DryRun prints what would happen and changes nothing.
	DryRun bool

	// Projects are the configured projects, if the config could be read.
	Projects []*config.Project

	Paths Paths
	In    io.Reader
	Out   io.Writer
	// Run executes a system command.
	Run func(name string, args ...string) error
	// IsRoot reports whether the process may modify system files.
	IsRoot bool
}

type step struct {
	desc string
	do   func() error
}

// Run performs the uninstall. It lists every action first and asks for
// confirmation, then carries them out in order, reporting failures at the end
// instead of stopping halfway.
func Run(o Options) error {
	if !o.IsRoot && !o.DryRun {
		return errors.New("uninstall must be run as root (use sudo)")
	}
	if o.DeleteProjects && !o.Purge {
		return errors.New("--delete-projects only makes sense together with --purge")
	}
	in := bufio.NewReader(o.In)

	steps, dirs, notes, err := o.plan()
	if err != nil {
		return err
	}
	fmt.Fprintln(o.Out, "This will:")
	for _, s := range steps {
		fmt.Fprintf(o.Out, "  - %s\n", s.desc)
	}
	if o.DryRun {
		fmt.Fprintln(o.Out, "\nDry run: nothing was changed.")
		printNotes(o.Out, notes)
		return nil
	}
	if !o.Yes && !confirm(in, o.Out, "\nType 'yes' to continue: ", "yes") {
		return errors.New("aborted")
	}
	if len(dirs) > 0 && !o.Yes {
		fmt.Fprintln(o.Out, "\nThese project directories will be deleted with everything in them:")
		for _, d := range dirs {
			fmt.Fprintf(o.Out, "  %s\n", d)
		}
		if !confirm(in, o.Out, "Type 'delete' to confirm: ", "delete") {
			return errors.New("aborted")
		}
	}

	deleteUser := false
	if o.Purge && !o.Yes {
		fmt.Fprintf(o.Out, "\nProject services may still run as user %q. ", UserName)
		deleteUser = confirm(in, o.Out, "Delete the user and its home directory too? [y/N] ", "y", "yes")
	}
	if deleteUser {
		steps = insertBeforeLast(steps, step{
			desc: fmt.Sprintf("delete user %s and its home directory", UserName),
			do:   func() error { return o.Run("userdel", "--remove", UserName) },
		})
	}

	var failed []string
	for _, s := range steps {
		fmt.Fprintf(o.Out, "→ %s\n", s.desc)
		if err := s.do(); err != nil {
			fmt.Fprintf(o.Out, "  ✘ %v\n", err)
			failed = append(failed, fmt.Sprintf("%s: %v", s.desc, err))
		}
	}
	printNotes(o.Out, notes)
	if len(failed) > 0 {
		return fmt.Errorf("%d step(s) failed:\n  %s", len(failed), strings.Join(failed, "\n  "))
	}
	fmt.Fprintln(o.Out, "\n✔ shipit has been removed")
	return nil
}

// plan builds the list of actions. Removing the binary is always last, so an
// interrupted uninstall can be repeated with the tool itself.
func (o Options) plan() (steps []step, dirs []string, notes []string, err error) {
	p := o.Paths

	if exists(p.Unit) {
		steps = append(steps,
			step{"stop and disable " + UnitName, func() error { return o.Run("systemctl", "disable", "--now", UnitName) }},
			step{"remove " + p.Unit, func() error { return remove(p.Unit) }},
			step{"reload systemd", func() error { return o.Run("systemctl", "daemon-reload") }},
		)
	}

	if o.Purge {
		steps = append(steps, step{"remove " + p.ConfigDir + " (config, secret, tokens)", func() error { return os.RemoveAll(p.ConfigDir) }})
	}

	if o.DeleteProjects {
		for _, proj := range o.Projects {
			if err := safeProjectDir(proj.Dir); err != nil {
				notes = append(notes, fmt.Sprintf("project %s: not deleting %s: %v", proj.Name, proj.Dir, err))
				continue
			}
			dirs = append(dirs, proj.Dir)
			steps = append(steps, step{"DELETE project directory " + proj.Dir, func() error { return os.RemoveAll(proj.Dir) }})
		}
		if len(o.Projects) == 0 {
			notes = append(notes, "no projects found in the config, so no project directories were deleted")
		}
	}

	if exists(p.Binary) {
		steps = append(steps, step{"remove " + p.Binary, func() error { return remove(p.Binary) }})
	}

	if len(steps) == 0 {
		return nil, nil, nil, errors.New("nothing to uninstall (shipit is not installed here)")
	}

	notes = append(notes, leftovers(o.Projects, len(dirs) > 0)...)
	return steps, dirs, notes, nil
}

func leftovers(projects []*config.Project, deletedDirs bool) []string {
	notes := []string{"Left for you to review (shipit never touches these):", "  /etc/sudoers.d/shipit"}
	for _, p := range projects {
		if p.Service != "" {
			notes = append(notes, fmt.Sprintf("  /etc/systemd/system/%s.service", p.Service))
		}
	}
	if !deletedDirs {
		for _, p := range projects {
			notes = append(notes, "  "+p.Dir+"  (project files)")
		}
	}
	return notes
}

func printNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}
}

// safeProjectDir refuses to delete anything that does not look like a
// directory shipit manages: it must be a real directory (not a symlink), not a
// well-known system location, and contain a releases/ directory.
func safeProjectDir(dir string) error {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || strings.Count(clean, "/") < 2 {
		return errors.New("too close to the filesystem root")
	}
	switch clean {
	case "/etc", "/usr", "/bin", "/sbin", "/lib", "/boot", "/dev", "/proc", "/sys", "/root",
		"/home", "/srv", "/opt", "/var", "/var/lib", "/var/www", "/tmp", "/mnt", "/media":
		return errors.New("a shared system directory")
	}
	fi, err := os.Lstat(clean)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("does not exist")
	case err != nil:
		return err
	case !fi.IsDir():
		return errors.New("not a real directory (symlink or file)")
	}
	if ri, err := os.Lstat(filepath.Join(clean, "releases")); err != nil || !ri.IsDir() {
		return errors.New("has no releases/ directory, so it does not look like a shipit project")
	}
	return nil
}

func confirm(in *bufio.Reader, out io.Writer, prompt string, accept ...string) bool {
	fmt.Fprint(out, prompt)
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	for _, a := range accept {
		if line == a {
			return true
		}
	}
	return false
}

func insertBeforeLast(steps []step, s step) []step {
	if len(steps) == 0 {
		return []step{s}
	}
	last := steps[len(steps)-1]
	return append(append(steps[:len(steps)-1:len(steps)-1], s), last)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

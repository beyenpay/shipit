// Package selfupdate replaces the installed shipit binary with a release from
// GitHub and restarts the service, restoring the old binary if the new one
// does not come up.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/release"
)

// Repo is where shipit releases are published.
const Repo = "beyenpay/shipit"

// UnitName is the systemd unit install.sh creates.
const UnitName = "shipit.service"

// Source is the part of the GitHub client self-update needs.
type Source interface {
	Release(ctx context.Context, repo, tag string) (*release.Release, error)
	DownloadVerified(ctx context.Context, repo string, rel *release.Release, name, dst string) error
}

type Options struct {
	// Version is the version of the running binary.
	Version string
	// Target is the tag to install; empty means the latest release.
	Target string
	DryRun bool

	// Binary is the installed executable to replace; Unit is its systemd unit.
	Binary string
	Unit   string
	// Arch is the server's CPU architecture (GOARCH).
	Arch   string
	IsRoot bool

	Out    io.Writer
	Source Source
	// Run executes a system command.
	Run func(name string, args ...string) error
	// Probe runs a binary's `version` command and returns the bare version.
	Probe func(path string) (string, error)
	// Settle is how long the service must stay up after a restart.
	Settle time.Duration
}

// Run performs the update. Nothing on disk changes until the new binary has
// been downloaded, checksum-verified and has proven it can run.
func Run(ctx context.Context, o Options) error {
	if !o.IsRoot && !o.DryRun {
		return errors.New("self-update must be run as root (use: sudo shipit self-update)")
	}
	if o.Target != "" && !config.ValidTag(o.Target) {
		return fmt.Errorf("invalid version %q", o.Target)
	}

	rel, err := o.Source.Release(ctx, Repo, o.Target)
	if err != nil {
		return err
	}
	if rel.Tag == o.Version {
		fmt.Fprintf(o.Out, "✔ already on %s\n", o.Version)
		return nil
	}
	fmt.Fprintf(o.Out, "updating %s -> %s\n", o.Version, rel.Tag)
	if o.DryRun {
		fmt.Fprintln(o.Out, "Dry run: nothing was changed.")
		return nil
	}

	if fi, err := os.Stat(o.Binary); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("shipit is not installed at %s (run install.sh first)", o.Binary)
	}

	fresh := o.Binary + ".new"
	old := o.Binary + ".old"
	_ = os.Remove(fresh)
	defer os.Remove(fresh) // a no-op once it has been renamed into place

	name := "shipit-linux-" + o.Arch
	fmt.Fprintf(o.Out, "→ downloading %s\n", name)
	if err := o.Source.DownloadVerified(ctx, Repo, rel, name, fresh); err != nil {
		return err
	}
	if err := os.Chmod(fresh, 0o755); err != nil {
		return err
	}

	got, err := o.Probe(fresh)
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run: %w", err)
	}
	if got != rel.Tag {
		return fmt.Errorf("the downloaded binary reports version %q, expected %q", got, rel.Tag)
	}

	// Keep the old binary next to the new one: a hard link is instant and the
	// rename below swaps the new one in atomically, so there is never a moment
	// without a working /usr/local/bin/shipit.
	_ = os.Remove(old)
	if err := os.Link(o.Binary, old); err != nil {
		return fmt.Errorf("keep a copy of the current binary: %w", err)
	}
	if err := os.Rename(fresh, o.Binary); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "→ installed %s (previous binary kept as %s)\n", o.Binary, old)

	if !o.restartNeeded() {
		fmt.Fprintf(o.Out, "✔ now on %s (%s is not running, so nothing was restarted)\n", rel.Tag, UnitName)
		return nil
	}
	fmt.Fprintf(o.Out, "→ restarting %s (waits for a running deploy to finish)\n", UnitName)
	if err := o.restart(); err != nil {
		fmt.Fprintf(o.Out, "✘ %v\n→ restoring the previous binary\n", err)
		if rerr := o.restore(old); rerr != nil {
			return fmt.Errorf("%w; restoring the previous version also failed: %v", err, rerr)
		}
		return fmt.Errorf("%w; the previous version was restored", err)
	}
	fmt.Fprintf(o.Out, "✔ now on %s\n", rel.Tag)
	return nil
}

// restartNeeded reports whether the service is currently running. An inactive
// service is left alone: starting it is not the updater's decision.
func (o Options) restartNeeded() bool {
	if _, err := os.Stat(o.Unit); err != nil {
		return false
	}
	return o.Run("systemctl", "is-active", "--quiet", UnitName) == nil
}

func (o Options) restart() error {
	if err := o.Run("systemctl", "restart", UnitName); err != nil {
		return fmt.Errorf("restart failed: %w", err)
	}
	time.Sleep(o.Settle)
	if err := o.Run("systemctl", "is-active", "--quiet", UnitName); err != nil {
		return fmt.Errorf("%s is not running after the update (see: journalctl -u shipit)", UnitName)
	}
	return nil
}

func (o Options) restore(old string) error {
	if err := os.Rename(old, o.Binary); err != nil {
		return err
	}
	if err := o.Run("systemctl", "restart", UnitName); err != nil {
		return err
	}
	return nil
}

// ParseVersionOutput extracts the version from the output of `shipit version`
// ("shipit v1.2.3").
func ParseVersionOutput(out string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "shipit"))
}

// Package deploy implements deploy and rollback for one project directory.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/release"
)

// DefaultTimeout is a sensible overall deadline for one deploy or rollback.
const DefaultTimeout = 10 * time.Minute

// Source is the part of the GitHub client the deployer needs.
type Source interface {
	Release(ctx context.Context, repo, tag string) (*release.Release, error)
	DownloadVerified(ctx context.Context, repo string, rel *release.Release, name, dst string) error
}

type Deployer struct {
	Cfg *config.Config
	// Arch is the server's CPU architecture (GOARCH).
	Arch string
	// Source builds a GitHub client for the given token.
	Source func(token string) Source
	// Restart restarts a systemd service.
	Restart func(ctx context.Context, service string) error
	// HTTP is used for health checks.
	HTTP         *http.Client
	PollInterval time.Duration
	// Logf receives progress messages. May be nil.
	Logf func(format string, args ...any)
}

func New(cfg *config.Config, logf func(string, ...any)) *Deployer {
	return &Deployer{
		Cfg:          cfg,
		Arch:         runtime.GOARCH,
		Source:       func(token string) Source { return release.NewClient(token) },
		Restart:      SystemctlRestart,
		HTTP:         &http.Client{Timeout: 5 * time.Second},
		PollInterval: time.Second,
		Logf:         logf,
	}
}

func (d *Deployer) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Deploy makes tag the live version of the project, downloading it first if
// it is not already on disk. An empty tag means the latest GitHub release.
// It returns the tag that was deployed.
func (d *Deployer) Deploy(ctx context.Context, name, tag string) (string, error) {
	p, err := d.project(name)
	if err != nil {
		return "", err
	}
	if tag != "" && !config.ValidTag(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	unlock, err := d.begin(p)
	if err != nil {
		return "", err
	}
	defer unlock()

	src := d.Source(d.Cfg.TokenFor(p))
	var rel *release.Release
	if tag == "" {
		d.logf("%s: resolving latest release of %s", p.Name, p.Repo)
		if rel, err = src.Release(ctx, p.Repo, ""); err != nil {
			return "", err
		}
		if tag = rel.Tag; !config.ValidTag(tag) {
			return "", fmt.Errorf("release tag %q cannot be deployed", tag)
		}
	}

	if hasRelease(p, tag) {
		d.logf("%s: %s is already on disk, skipping download", p.Name, tag)
	} else {
		if rel == nil {
			d.logf("%s: resolving release %s", p.Name, tag)
			if rel, err = src.Release(ctx, p.Repo, tag); err != nil {
				return "", err
			}
		}
		if err := d.install(ctx, p, src, rel, tag); err != nil {
			return "", err
		}
	}

	if err := d.activate(ctx, p, tag); err != nil {
		return tag, err
	}
	d.cleanup(p)
	return tag, nil
}

// Rollback switches the project to a release that is already on disk: the
// given tag, or the previous version if tag is empty. It never downloads.
func (d *Deployer) Rollback(ctx context.Context, name, tag string) (string, error) {
	p, err := d.project(name)
	if err != nil {
		return "", err
	}
	if tag != "" && !config.ValidTag(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	unlock, err := d.begin(p)
	if err != nil {
		return "", err
	}
	defer unlock()

	cur, err := readTag(p, currentName)
	if err != nil {
		return "", err
	}
	if tag == "" {
		prev, err := readTag(p, previousName)
		if err != nil {
			return "", err
		}
		switch {
		case prev == "":
			return "", errors.New("no previous version to roll back to")
		case prev == cur:
			return "", fmt.Errorf("previous version is the same as current (%s); nothing to roll back to", cur)
		}
		tag = prev
	} else if tag == cur {
		return "", fmt.Errorf("%s is already the current version", tag)
	}
	if !hasRelease(p, tag) {
		return "", fmt.Errorf("release %s is not on this server; fetch it with `shipit deploy %s %s`", tag, p.Name, tag)
	}

	if err := d.activate(ctx, p, tag); err != nil {
		return tag, err
	}
	d.cleanup(p)
	return tag, nil
}

type Status struct {
	Current  string
	Previous string
}

func (d *Deployer) Status(name string) (Status, error) {
	p, err := d.project(name)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if s.Current, err = readTag(p, currentName); err != nil {
		return s, err
	}
	s.Previous, err = readTag(p, previousName)
	return s, err
}

// Releases lists the tags on disk, most recently used first.
func (d *Deployer) Releases(name string) ([]string, error) {
	p, err := d.project(name)
	if err != nil {
		return nil, err
	}
	infos, err := listReleases(p)
	if err != nil {
		return nil, err
	}
	tags := make([]string, len(infos))
	for i, r := range infos {
		tags[i] = r.Tag
	}
	return tags, nil
}

func (d *Deployer) project(name string) (*config.Project, error) {
	p, ok := d.Cfg.Projects[name]
	if !ok {
		return nil, fmt.Errorf("unknown project %q (known: %s)", name, strings.Join(d.Cfg.Names(), ", "))
	}
	return p, nil
}

// begin prepares the project directory and takes the project lock. It also
// refuses to continue if the links are not ours, and removes leftovers of
// interrupted downloads (safe, because we hold the lock).
func (d *Deployer) begin(p *config.Project) (unlock func(), err error) {
	if fi, err := os.Stat(p.Dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s: dir %s does not exist (create it and give it to the shipit user)", p.Name, p.Dir)
	}
	if err := os.MkdirAll(releasesDir(p), 0o755); err != nil {
		return nil, err
	}
	l, err := acquire(filepath.Join(p.Dir, lockName))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	for _, link := range []string{currentName, previousName} {
		if _, err := readTag(p, link); err != nil {
			l.release()
			return nil, err
		}
	}
	d.removeStale(p)
	return l.release, nil
}

func (d *Deployer) removeStale(p *config.Project) {
	entries, err := os.ReadDir(releasesDir(p))
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || strings.HasPrefix(e.Name(), ".dl-") {
			d.logf("%s: removing leftover %s", p.Name, e.Name())
			_ = os.RemoveAll(filepath.Join(releasesDir(p), e.Name()))
		}
	}
}

// install downloads, verifies and unpacks tag into releases/<tag>. The release
// only appears under its final name once it is complete and has passed the
// type checks, so releases/<tag> existing always means "usable".
func (d *Deployer) install(ctx context.Context, p *config.Project, src Source, rel *release.Release, tag string) (err error) {
	rd := releasesDir(p)
	archive := filepath.Join(rd, ".dl-"+tag+".tar.gz")
	tmp := filepath.Join(rd, ".tmp-"+tag)
	defer func() {
		_ = os.Remove(archive)
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()

	name := p.AssetName(tag, d.Arch)
	d.logf("%s: downloading %s", p.Name, name)
	if err := src.DownloadVerified(ctx, p.Repo, rel, name, archive); err != nil {
		return err
	}
	d.logf("%s: checksum ok, extracting", p.Name)
	if err := release.Extract(archive, tmp); err != nil {
		return err
	}
	if err := verifyRelease(p, tmp, d.Arch); err != nil {
		return fmt.Errorf("release %s rejected: %w", tag, err)
	}
	return os.Rename(tmp, releaseDir(p, tag))
}

// activate points current at tag, restarts the service and waits for the
// health check. If that fails and there was a working version before, it is
// restored. The previous link is always written before current, so a crash
// between the two steps leaves previous == current, which is harmless.
func (d *Deployer) activate(ctx context.Context, p *config.Project, tag string) error {
	if err := verifyRelease(p, releaseDir(p, tag), d.Arch); err != nil {
		return fmt.Errorf("release %s rejected: %w", tag, err)
	}
	cur, err := readTag(p, currentName)
	if err != nil {
		return err
	}
	prev, err := readTag(p, previousName)
	if err != nil {
		return err
	}
	// Only a release that is still on disk can be restored.
	canRestore := cur != "" && cur != tag && hasRelease(p, cur)

	if cur == tag {
		d.logf("%s: %s is already current, restarting", p.Name, tag)
	} else {
		if canRestore {
			if err := setLink(p, previousName, cur); err != nil {
				return err
			}
		}
		if err := setLink(p, currentName, tag); err != nil {
			return err
		}
		d.logf("%s: current %s -> %s", p.Name, orNone(cur), tag)
	}

	if err := d.restartAndCheck(ctx, p); err != nil {
		if !canRestore {
			return fmt.Errorf("%w (no earlier version to roll back to; %s left in place)", err, tag)
		}
		d.logf("%s: FAILED (%v), rolling back to %s", p.Name, err, cur)
		// The rollback must finish even if the caller gave up.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute+p.HealthTimeout)
		defer cancel()
		if rerr := d.restore(rctx, p, cur, prev); rerr != nil {
			return fmt.Errorf("%w; rolling back to %s also failed: %v", err, cur, rerr)
		}
		return fmt.Errorf("%w; rolled back to %s", err, cur)
	}

	now := time.Now()
	_ = os.Chtimes(releaseDir(p, tag), now, now) // marks it as recently used for cleanup
	return nil
}

func (d *Deployer) restore(ctx context.Context, p *config.Project, cur, prev string) error {
	if err := setLink(p, currentName, cur); err != nil {
		return err
	}
	var err error
	if prev != "" && hasRelease(p, prev) {
		err = setLink(p, previousName, prev)
	} else {
		err = removeLink(p, previousName)
	}
	if err != nil {
		return err
	}
	return d.restartAndCheck(ctx, p)
}

func (d *Deployer) restartAndCheck(ctx context.Context, p *config.Project) error {
	if p.Service != "" {
		d.logf("%s: restarting %s", p.Name, p.Service)
		if err := d.Restart(ctx, p.Service); err != nil {
			return err
		}
	}
	if p.Health != "" {
		d.logf("%s: waiting for %s (up to %s)", p.Name, p.Health, p.HealthTimeout)
		if err := d.waitHealthy(ctx, p); err != nil {
			return err
		}
		d.logf("%s: healthy", p.Name)
	}
	return nil
}

// cleanup removes old releases beyond the project's keep limit. The current
// and previous releases are never removed. Failures are only logged: the
// deploy itself already succeeded.
func (d *Deployer) cleanup(p *config.Project) {
	infos, err := listReleases(p)
	if err != nil {
		d.logf("%s: cleanup skipped: %v", p.Name, err)
		return
	}
	protected := map[string]bool{}
	for _, link := range []string{currentName, previousName} {
		if tag, err := readTag(p, link); err == nil && tag != "" {
			protected[tag] = true
		}
	}
	kept := 0
	for _, r := range infos {
		if protected[r.Tag] {
			kept++
		}
	}
	for _, r := range infos { // most recently used first
		if protected[r.Tag] {
			continue
		}
		if kept < p.Keep {
			kept++
			continue
		}
		d.logf("%s: removing old release %s", p.Name, r.Tag)
		if err := os.RemoveAll(releaseDir(p, r.Tag)); err != nil {
			d.logf("%s: cannot remove %s: %v", p.Name, r.Tag, err)
		}
	}
}

func orNone(tag string) string {
	if tag == "" {
		return "(none)"
	}
	return tag
}

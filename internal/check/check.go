// Package check inspects the configuration and the machine it runs on and
// reports what would stop a deploy from working. It never changes anything.
package check

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/deploy"
	"github.com/beyenpay/shipit/internal/release"
)

type Level int

const (
	OK Level = iota
	Warn
	Fail
)

type Line struct {
	Level Level
	Text  string
}

type Section struct {
	Title string
	Lines []Line
}

// Remote is the part of the GitHub client that check needs.
type Remote interface {
	TokenStatus(ctx context.Context) (expires string, err error)
	CheckRepo(ctx context.Context, repo string) error
}

type Checker struct {
	Cfg     *config.Config
	Path    string
	Offline bool
	// User is the account the project services must run as.
	User string

	// The fields below exist so tests can replace the outside world.
	Run    func(ctx context.Context, name string, args ...string) (string, error)
	Listen func(addr string) error
	Remote func(token string) Remote
	Now    func() time.Time
}

func New(cfg *config.Config, path string, offline bool) *Checker {
	return &Checker{
		Cfg: cfg, Path: path, Offline: offline, User: "shipit",
		Run:    run,
		Listen: tryListen,
		Remote: func(token string) Remote { return release.NewClient(token) },
		Now:    time.Now,
	}
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func tryListen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// All runs every check and returns the results, one section per topic.
func (c *Checker) All(ctx context.Context) []Section {
	secs := []Section{c.global(ctx)}
	for _, name := range c.Cfg.Names() {
		secs = append(secs, c.project(ctx, c.Cfg.Projects[name]))
	}
	return secs
}

// Print writes the results and returns the number of failures.
func Print(w io.Writer, secs []Section) (failures int) {
	mark := map[Level]string{OK: "✔", Warn: "!", Fail: "✘"}
	for _, s := range secs {
		fmt.Fprintln(w, s.Title)
		for _, l := range s.Lines {
			fmt.Fprintf(w, "  %s %s\n", mark[l.Level], l.Text)
			if l.Level == Fail {
				failures++
			}
		}
	}
	return failures
}

type builder struct{ s Section }

func (b *builder) add(l Level, format string, a ...any) {
	b.s.Lines = append(b.s.Lines, Line{l, fmt.Sprintf(format, a...)})
}

func (c *Checker) global(ctx context.Context) Section {
	b := &builder{Section{Title: "shipit"}}

	if err := config.CheckFileMode(c.Path); err != nil {
		b.add(Fail, "%v", err)
	} else {
		b.add(OK, "config %s (mode 0600)", c.Path)
	}
	if err := c.Cfg.ValidateServe(); err != nil {
		b.add(Fail, "%v", err)
	}
	if err := c.Listen(c.Cfg.Listen); err != nil {
		b.add(Warn, "cannot listen on %s: %v (expected if shipit.service is already running)", c.Cfg.Listen, err)
	} else {
		b.add(OK, "port %s is free", c.Cfg.Listen)
	}

	missing := false
	for _, p := range c.Cfg.Projects {
		if c.Cfg.TokenFor(p) == "" {
			missing = true
		}
	}
	switch {
	case c.Cfg.Token == "" && missing:
		b.add(Warn, "no GitHub token for some projects: only public repositories work, with a low API rate limit")
	case c.Cfg.Token != "" && !c.Offline:
		c.token(ctx, b, "global token", c.Cfg.Token)
	}
	if len(c.Cfg.Projects) == 0 {
		b.add(Warn, "no projects configured yet")
	}
	return b.s
}

func (c *Checker) token(ctx context.Context, b *builder, what, token string) {
	expires, err := c.Remote(token).TokenStatus(ctx)
	if err != nil {
		b.add(Fail, "%s: %v", what, err)
		return
	}
	if expires == "" {
		b.add(OK, "%s valid (no expiry)", what)
		return
	}
	t, perr := time.Parse("2006-01-02 15:04:05 MST", expires)
	switch {
	case perr != nil:
		b.add(OK, "%s valid (expires %s)", what, expires)
	case t.Sub(c.Now()) < 14*24*time.Hour:
		b.add(Warn, "%s expires soon: %s", what, expires)
	default:
		b.add(OK, "%s valid (expires %s)", what, t.Format("2006-01-02"))
	}
}

func (c *Checker) project(ctx context.Context, p *config.Project) Section {
	b := &builder{Section{Title: p.Name}}
	b.add(OK, "type=%s repo=%s", p.Type, p.Repo)

	c.dir(b, p)
	if !c.Offline {
		if p.Token != "" {
			c.token(ctx, b, "project token", p.Token)
		}
		if err := c.Remote(c.Cfg.TokenFor(p)).CheckRepo(ctx, p.Repo); err != nil {
			b.add(Fail, "repo %s: %v", p.Repo, err)
		} else {
			b.add(OK, "repo %s reachable", p.Repo)
		}
	}
	if p.Service != "" {
		c.unit(ctx, b, p)
		c.sudoers(ctx, b, p)
	}
	return b.s
}

func (c *Checker) dir(b *builder, p *config.Project) {
	fi, err := os.Stat(p.Dir)
	if err != nil || !fi.IsDir() {
		b.add(Fail, "dir %s does not exist; create it and give it to %s", p.Dir, c.User)
		return
	}
	f, err := os.CreateTemp(p.Dir, ".shipit-check-*")
	if err != nil {
		b.add(Fail, "dir %s is not writable by this user: %v", p.Dir, err)
		return
	}
	f.Close()
	os.Remove(f.Name())
	b.add(OK, "dir %s writable", p.Dir)
}

// unit verifies the hand-written systemd unit: it exists, runs as the shipit
// user (never root, since shipit can write the code it executes) and starts in
// <dir>/current.
func (c *Checker) unit(ctx context.Context, b *builder, p *config.Project) {
	out, err := c.Run(ctx, "systemctl", "show", p.Service, "-p", "LoadState", "-p", "User", "-p", "WorkingDirectory")
	if err != nil {
		b.add(Fail, "cannot query systemd for %q: %v", p.Service, err)
		return
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] != "loaded" {
		b.add(Fail, "unit %s.service not found (LoadState=%s)", p.Service, props["LoadState"])
		return
	}
	switch u := props["User"]; u {
	case c.User:
		b.add(OK, "unit %s.service found, User=%s", p.Service, u)
	case "":
		b.add(Fail, "unit %s.service has no User= and would run as root; set User=%s", p.Service, c.User)
	default:
		b.add(Fail, "unit %s.service runs as %q, expected %q", p.Service, u, c.User)
	}
	want := filepath.Join(p.Dir, "current")
	if got := props["WorkingDirectory"]; filepath.Clean(got) != want {
		b.add(Fail, "unit WorkingDirectory=%s, expected %s", got, want)
	} else {
		b.add(OK, "WorkingDirectory=%s", got)
	}
}

// sudoers asks sudo whether the exact restart command is allowed. `sudo -l
// <command>` only answers the question; it does not run anything.
func (c *Checker) sudoers(ctx context.Context, b *builder, p *config.Project) {
	out, err := c.Run(ctx, "sudo", "-n", "-l", deploy.SystemctlPath, "restart", p.Service)
	if err != nil {
		b.add(Fail, "sudoers does not allow `%s restart %s` without a password: %s", deploy.SystemctlPath, p.Service, firstLine(out))
		return
	}
	b.add(OK, "sudoers allows restart")
}

func firstLine(s string) string {
	if s == "" {
		return "(no output)"
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}

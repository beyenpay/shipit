// Package config loads and validates shipit.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Type is the kind of project; it decides the asset naming and the sanity
// checks applied to a release before it goes live.
type Type string

const (
	TypeGo   Type = "go"
	TypeVite Type = "vite"
	TypeNext Type = "next"
)

const (
	DefaultListen        = ":9000"
	DefaultKeep          = 5
	MinKeep              = 2 // current + previous are always kept
	DefaultHealthTimeout = 30 * time.Second
	MaxHealthTimeout     = 10 * time.Minute
	MinSecretLen         = 16
)

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	tagRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Config struct {
	Listen   string              `yaml:"listen"`
	Secret   string              `yaml:"secret"`
	Token    string              `yaml:"token"`
	Projects map[string]*Project `yaml:"projects"`
}

type Project struct {
	Name          string        `yaml:"-"`
	Type          Type          `yaml:"type"`
	Repo          string        `yaml:"repo"`
	Dir           string        `yaml:"dir"`
	Service       string        `yaml:"service"`
	Health        string        `yaml:"health"`
	Asset         string        `yaml:"asset"`
	Keep          int           `yaml:"keep"`
	Token         string        `yaml:"token"`
	HealthTimeout time.Duration `yaml:"health_timeout"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates a config document. Unknown fields are errors.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Names returns the project names in sorted order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TokenFor returns the GitHub token to use for p: the project's own token,
// falling back to the global one. It may be empty (public repositories).
func (c *Config) TokenFor(p *Project) string {
	if p.Token != "" {
		return p.Token
	}
	return c.Token
}

// ValidateServe checks the settings that only the webhook server needs.
func (c *Config) ValidateServe() error {
	if len(c.Secret) < MinSecretLen {
		return fmt.Errorf("secret must be at least %d characters", MinSecretLen)
	}
	return nil
}

// AssetName returns the release asset file name expected for tag on the given
// CPU architecture (GOARCH). vite builds are architecture independent.
func (p *Project) AssetName(tag, arch string) string {
	if p.Type == TypeVite {
		return fmt.Sprintf("%s-%s-any.tar.gz", p.Asset, tag)
	}
	return fmt.Sprintf("%s-%s-linux-%s.tar.gz", p.Asset, tag, arch)
}

// ValidTag reports whether tag is safe to use as a release directory name and
// in URLs. It cannot start with '.', so it can never be "." or "..".
func ValidTag(tag string) bool {
	return tagRe.MatchString(tag)
}

// CheckFileMode fails if the config file is readable by group or others,
// since it holds the webhook secret and GitHub tokens.
func CheckFileMode(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o, expected 0600 (it contains secrets)", path, perm)
	}
	return nil
}

func (c *Config) normalize() error {
	var errs []error

	c.Secret = strings.TrimSpace(c.Secret)
	c.Token = strings.TrimSpace(c.Token)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := validListen(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("listen: %w", err))
	}

	for _, name := range c.Names() {
		p := c.Projects[name]
		if p == nil {
			errs = append(errs, fmt.Errorf("project %q: empty definition", name))
			continue
		}
		p.Name = name
		errs = append(errs, p.normalize()...)
	}
	errs = append(errs, c.checkConflicts()...)

	return errors.Join(errs...)
}

func (p *Project) normalize() []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("project %q: %s", p.Name, fmt.Sprintf(format, a...)))
	}

	if !nameRe.MatchString(p.Name) {
		bad("invalid name (letters, digits, '.', '_', '-'; max 64 chars)")
	}

	switch p.Type {
	case TypeGo, TypeNext:
		if p.Service == "" {
			bad("service is required for type %q", p.Type)
		}
	case TypeVite:
		if p.Service != "" {
			bad("service must not be set for type %q (a static site has no process)", p.Type)
		}
	case "":
		bad("type is required (go | vite | next)")
	default:
		bad("unknown type %q (go | vite | next)", p.Type)
	}
	if p.Service != "" && !serviceRe.MatchString(p.Service) {
		bad("invalid service name %q", p.Service)
	}

	if !repoRe.MatchString(p.Repo) {
		bad("repo must look like owner/name, got %q", p.Repo)
	}

	switch {
	case p.Dir == "":
		bad("dir is required")
	case !filepath.IsAbs(p.Dir):
		bad("dir must be an absolute path, got %q", p.Dir)
	default:
		p.Dir = filepath.Clean(p.Dir)
		if p.Dir == "/" {
			bad("dir must not be /")
		}
	}

	if p.Asset == "" {
		p.Asset = p.Name
	} else if !nameRe.MatchString(p.Asset) {
		bad("invalid asset %q", p.Asset)
	}

	switch {
	case p.Keep == 0:
		p.Keep = DefaultKeep
	case p.Keep < MinKeep:
		bad("keep must be >= %d (current and previous are always kept)", MinKeep)
	}

	if p.Health != "" {
		u, err := url.Parse(p.Health)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("health must be an http(s) URL, got %q", p.Health)
		}
	}
	switch {
	case p.HealthTimeout == 0:
		p.HealthTimeout = DefaultHealthTimeout
	case p.HealthTimeout < 0 || p.HealthTimeout > MaxHealthTimeout:
		bad("health_timeout must be > 0 and <= %s", MaxHealthTimeout)
	}

	p.Token = strings.TrimSpace(p.Token)
	return errs
}

// checkConflicts catches projects that would step on each other.
func (c *Config) checkConflicts() []error {
	var errs []error
	names := c.Names()
	for i, a := range names {
		pa := c.Projects[a]
		if pa == nil {
			continue
		}
		for _, b := range names[i+1:] {
			pb := c.Projects[b]
			if pb == nil {
				continue
			}
			if pa.Dir != "" && pb.Dir != "" && overlaps(pa.Dir, pb.Dir) {
				errs = append(errs, fmt.Errorf("projects %q and %q: dir overlaps (%s, %s)", a, b, pa.Dir, pb.Dir))
			}
			if pa.Service != "" && pa.Service == pb.Service {
				errs = append(errs, fmt.Errorf("projects %q and %q: share service %q", a, b, pa.Service))
			}
		}
	}
	return errs
}

func overlaps(a, b string) bool {
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

func validListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

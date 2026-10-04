package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// proj builds a config with a single project named "app" from the given
// "key: value" lines.
func proj(fields string) string {
	var b strings.Builder
	b.WriteString("projects:\n  app:\n")
	for _, l := range strings.Split(strings.TrimSpace(fields), "\n") {
		b.WriteString("    " + strings.TrimSpace(l) + "\n")
	}
	return b.String()
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(proj("type: next\nrepo: beyenpay/home\ndir: /srv/app/beyen-home/web/\nservice: beyen-home")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" {
		t.Errorf("Listen = %q", c.Listen)
	}
	p := c.Projects["app"]
	if p.Name != "app" || p.Asset != "app" {
		t.Errorf("Name/Asset = %q/%q", p.Name, p.Asset)
	}
	if p.Dir != "/srv/app/beyen-home/web" {
		t.Errorf("Dir not cleaned: %q", p.Dir)
	}
	if p.Keep != 5 || p.HealthTimeout != 30*time.Second {
		t.Errorf("Keep/HealthTimeout = %d/%s", p.Keep, p.HealthTimeout)
	}
}

func TestParseEmpty(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || len(c.Projects) != 0 {
		t.Errorf("unexpected config: %+v", c)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"unknown field", proj("type: go\nrepo: o/r\ndir: /srv/app\nservice: app\nservce: x"), "servce"},
		{"missing type", proj("repo: o/r\ndir: /srv/app"), "type is required"},
		{"unknown type", proj("type: ruby\nrepo: o/r\ndir: /srv/app"), "unknown type"},
		{"go without service", proj("type: go\nrepo: o/r\ndir: /srv/app"), "service is required"},
		{"next without service", proj("type: next\nrepo: o/r\ndir: /srv/app"), "service is required"},
		{"vite with service", proj("type: vite\nrepo: o/r\ndir: /srv/app\nservice: app"), "must not be set"},
		{"bad service", proj("type: go\nrepo: o/r\ndir: /srv/app\nservice: \"--now\""), "invalid service"},
		{"bad repo", proj("type: vite\nrepo: justname\ndir: /srv/app"), "owner/name"},
		{"relative dir", proj("type: vite\nrepo: o/r\ndir: srv/app"), "absolute"},
		{"root dir", proj("type: vite\nrepo: o/r\ndir: /"), "must not be /"},
		{"keep too small", proj("type: vite\nrepo: o/r\ndir: /srv/app\nkeep: 1"), "keep must be"},
		{"bad health", proj("type: vite\nrepo: o/r\ndir: /srv/app\nhealth: ftp://x"), "health must be"},
		{"health timeout too large", proj("type: vite\nrepo: o/r\ndir: /srv/app\nhealth_timeout: 1h"), "health_timeout"},
		{"bad listen", "listen: \"9000\"\n", "listen"},
		{"bad name", "projects:\n  \"bad name\":\n    type: vite\n    repo: o/r\n    dir: /srv/x\n", "invalid name"},
		{"empty project", "projects:\n  app:\n", "empty definition"},
		{
			"overlapping dirs",
			"projects:\n  a: {type: vite, repo: o/a, dir: /srv/app}\n  b: {type: vite, repo: o/b, dir: /srv/app/b}\n",
			"dir overlaps",
		},
		{
			"shared service",
			"projects:\n  a: {type: go, repo: o/a, dir: /srv/a, service: svc}\n  b: {type: go, repo: o/b, dir: /srv/b, service: svc}\n",
			"share service",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestNamesSorted(t *testing.T) {
	c, err := Parse([]byte("projects:\n  b: {type: vite, repo: o/b, dir: /srv/b}\n  a: {type: vite, repo: o/a, dir: /srv/a}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Names() = %v", got)
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct {
		p    Project
		want string
	}{
		{Project{Type: TypeGo, Asset: "beyen-api"}, "beyen-api-v1.2.0-linux-amd64.tar.gz"},
		{Project{Type: TypeNext, Asset: "beyen-home"}, "beyen-home-v1.2.0-linux-amd64.tar.gz"},
		{Project{Type: TypeVite, Asset: "beyen-web"}, "beyen-web-v1.2.0-any.tar.gz"},
	}
	for _, tt := range tests {
		if got := tt.p.AssetName("v1.2.0", "amd64"); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.p.Type, got, tt.want)
		}
	}
}

func TestValidTag(t *testing.T) {
	for _, tag := range []string{"v1.2.0", "1.0", "release_1", "v1.0.0-rc.1"} {
		if !ValidTag(tag) {
			t.Errorf("ValidTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range []string{"", "../x", "v1/2", ".hidden", "..", "v1 2", "v1;rm", "-v1"} {
		if ValidTag(tag) {
			t.Errorf("ValidTag(%q) = true, want false", tag)
		}
	}
}

func TestTokenFor(t *testing.T) {
	c := &Config{Token: "global"}
	if got := c.TokenFor(&Project{}); got != "global" {
		t.Errorf("got %q, want global", got)
	}
	if got := c.TokenFor(&Project{Token: "own"}); got != "own" {
		t.Errorf("got %q, want own", got)
	}
}

func TestValidateServe(t *testing.T) {
	if err := (&Config{Secret: "short"}).ValidateServe(); err == nil {
		t.Error("short secret accepted")
	}
	if err := (&Config{Secret: strings.Repeat("x", MinSecretLen)}).ValidateServe(); err != nil {
		t.Error(err)
	}
}

func TestCheckFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shipit.yaml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if CheckFileMode(path) == nil {
		t.Error("0644 accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileMode(path); err != nil {
		t.Error(err)
	}
}

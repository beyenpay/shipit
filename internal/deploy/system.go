package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/identity"
)

const (
	sudoBin = "/usr/bin/sudo"
	// SystemctlPath is the only command shipit runs through sudo; the
	// sudoers rule for each service must name exactly this path.
	SystemctlPath = "/usr/bin/systemctl"
)

// SystemctlRestart restarts a service with `sudo -n systemctl restart`. The
// command is fixed and run without a shell; sudoers must allow exactly this
// command for this service.
//
// Before issuing the restart it verifies that the unit does not run as root.
// shipit writes the code a unit executes, so a unit that ran as root would turn
// a malicious release into root execution through the restart; refusing early
// makes that impossible even when the sudoers rule and the unit file are both
// present but the User= line is missing or wrong. Any other account is fine:
// giving every service its own user keeps one compromised service away from
// the others. systemctl show does not need sudo.
func SystemctlRestart(ctx context.Context, service string) error {
	if service == "" || strings.HasPrefix(service, "-") {
		return fmt.Errorf("invalid service name %q", service)
	}
	props, err := unitProps(ctx, service, "LoadState", "User")
	if err != nil {
		return fmt.Errorf("cannot inspect unit %s before restart: %w", service, err)
	}
	if state := props["LoadState"]; state != "loaded" {
		return fmt.Errorf("unit %s is not loaded (LoadState=%s)", service, state)
	}
	if err := ValidateRunner(props["User"]); err != nil {
		return fmt.Errorf("refusing to restart %s: %w; give the unit its own non-root User= and run systemctl daemon-reload", service, err)
	}
	out, err := exec.CommandContext(ctx, sudoBin, "-n", SystemctlPath, "restart", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo systemctl restart %s: %w: %s", service, err, clip(out))
	}
	return nil
}

// Lookups of accounts, replaceable in tests.
var (
	userByName = user.Lookup
	userByID   = user.LookupId
)

// ValidateRunner reports whether name, the User= of a unit as systemd shows
// it, is an account a project service may run as: it must be set, exist on this
// machine and not be root. The empty string is what systemd reports for a unit
// without User=, which runs as root. Names and numeric ids are both accepted,
// since systemd accepts both.
func ValidateRunner(name string) error {
	switch name {
	case "":
		return errors.New("unit has no User= set, so it would run as root")
	case "root", "0":
		return errors.New("unit runs as root")
	}
	lookup := userByName
	if isDigits(name) {
		lookup = userByID
	}
	u, err := lookup(name)
	if err != nil {
		return fmt.Errorf("cannot resolve User=%s: %w", name, err)
	}
	if u.Uid == "0" {
		return fmt.Errorf("unit runs as %q, which is root", name)
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Runner returns the account a service's unit runs as: its User=, "root" when
// the unit sets none, or "n/a" when that cannot be determined (no such unit,
// systemctl failing).
func Runner(ctx context.Context, service string) string {
	if service == "" || strings.HasPrefix(service, "-") {
		return "n/a"
	}
	props, err := unitProps(ctx, service, "LoadState", "User")
	if err != nil || props["LoadState"] != "loaded" {
		return "n/a"
	}
	if u := props["User"]; u != "" {
		return u
	}
	return "root"
}

// RunnerLabel marks the accounts a service should no longer run as: root, and
// shipit itself, whose compromise would reach every project that shares it.
func RunnerLabel(runner string) string {
	switch runner {
	case "root", "0", identity.ServiceUser:
		return runner + " !"
	}
	return runner
}

// unitProps returns the named properties of a unit as `systemctl show`
// reports them. A property systemd leaves out is missing from the map.
func unitProps(ctx context.Context, service string, props ...string) (map[string]string, error) {
	args := []string{"show", service}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, err := exec.CommandContext(ctx, SystemctlPath, args...).Output()
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m, nil
}

// ServiceState returns the output of `systemctl is-active` ("active",
// "inactive", "failed", ...), or "n/a" if it cannot be determined.
func ServiceState(ctx context.Context, service string) string {
	if service == "" || strings.HasPrefix(service, "-") {
		return "n/a"
	}
	out, _ := exec.CommandContext(ctx, SystemctlPath, "is-active", service).Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "n/a"
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}

// waitHealthy polls the project's health URL until it answers 2xx or the
// project's health timeout expires. Redirects are followed.
func (d *Deployer) waitHealthy(ctx context.Context, p *config.Project) error {
	ctx, cancel := context.WithTimeout(ctx, p.HealthTimeout)
	defer cancel()
	for {
		err := d.probe(ctx, p.Health)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy after %s: %w", p.HealthTimeout, err)
		case <-time.After(d.PollInterval):
		}
	}
}

func (d *Deployer) probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "shipit-health")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

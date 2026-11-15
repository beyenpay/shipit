package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
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
// Before issuing the restart it verifies that the unit runs as the shipit user.
// A unit misconfigured to run as root would let a malicious release gain root
// execution through the restart; refusing early makes that impossible even if
// the sudoers rule and the unit file are both present but the User= line is
// wrong. systemctl show does not need sudo.
func SystemctlRestart(ctx context.Context, service string) error {
	if service == "" || strings.HasPrefix(service, "-") {
		return fmt.Errorf("invalid service name %q", service)
	}
	show, err := exec.CommandContext(ctx, SystemctlPath, "show", service, "-p", "User").Output()
	if err != nil {
		return fmt.Errorf("cannot inspect unit %s before restart: %w", service, err)
	}
	u := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(show)), "User="))
	switch {
	case u == "":
		return fmt.Errorf("unit %s has no User= set (would run as root); add User=%s to the unit file and run systemctl daemon-reload", service, identity.ServiceUser)
	case u != identity.ServiceUser:
		return fmt.Errorf("unit %s runs as %q, not %q; set User=%s in the unit file and run systemctl daemon-reload", service, u, identity.ServiceUser, identity.ServiceUser)
	}
	out, err := exec.CommandContext(ctx, sudoBin, "-n", SystemctlPath, "restart", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo systemctl restart %s: %w: %s", service, err, clip(out))
	}
	return nil
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

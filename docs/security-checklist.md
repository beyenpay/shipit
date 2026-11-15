# shipit Security Checklist

Use this document when **installing shipit for the first time** or **adding a new project**.
Work through every item in order. Items marked **[auto]** are already verified by `shipit check`;
the rest require manual inspection.

---

## Part 1 — What `shipit check` already covers

Run it first. Fix every `✘` before continuing.

```bash
sudo shipit check
```

| Check | What it verifies |
|-------|-----------------|
| Config file mode | `/etc/shipit/shipit.yaml` is mode `0600` (not readable by other local users) |
| Secret strength | `secret` is at least 16 characters |
| Webhook port | Whether the port is free, occupied by shipit, or taken by another process |
| GitHub token | Token is valid; warns if it expires within 14 days |
| Project directory | Directory exists and is writable by the running user |
| GitHub repository | Repo is reachable with the configured token |
| Systemd unit | Unit is loaded; `User=shipit`; `WorkingDirectory=<dir>/current` |
| Sudoers rule | `sudo -n systemctl restart <service>` is allowed without a password |

**Gaps — things `shipit check` does not verify:**

- Project directory mode (`0750` vs world-readable `0755`)
- `shared/.env` existence and mode
- `/etc/sudoers.d/shipit` file mode
- Node.js runtime present (required for `type: next`)
- `www-data` group membership (required for `type: vite` with nginx)
- TLS / network exposure of the webhook port

The manual steps below cover these gaps.

---

## Part 2 — Installation checklist (once per server)

### 2.1 shipit binary and service

```bash
# Verify binary ownership and mode
stat /usr/local/bin/shipit
# Expected: Uid: root  Mode: 0755
```

- [ ] `/usr/local/bin/shipit` — owner `root:root`, mode `0755`
- [ ] `/etc/systemd/system/shipit.service` — owner `root:root`, mode `0644`
- [ ] `shipit.service` has `User=shipit` (never `User=root`)
- [ ] `shipit.service` does **not** have `NoNewPrivileges=yes` (shipit needs sudo to restart project services)

### 2.2 shipit user and config directory

```bash
id shipit                              # user exists, shell is /usr/sbin/nologin
stat /etc/shipit
stat /etc/shipit/shipit.yaml
stat /var/lib/shipit
```

- [ ] `shipit` user exists with a non-login shell (`/usr/sbin/nologin` or `/bin/false`)
- [ ] `/etc/shipit/` — owner `shipit:shipit`, mode `0700`
- [ ] `/etc/shipit/shipit.yaml` — owner `shipit:shipit`, mode `0600`
- [ ] `/var/lib/shipit/` — owner `shipit:shipit`, mode `0700`

### 2.3 Sudoers

```bash
stat /etc/sudoers.d/shipit
cat  /etc/sudoers.d/shipit
```

- [ ] `/etc/sudoers.d/shipit` — owner `root:root`, mode `0440`
- [ ] Every line specifies an **exact service name** — no wildcards, no bare `/usr/bin/systemctl`

```
# Correct — exact command, exact service
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home

# Wrong — wildcard; lets shipit restart any service, including root-owned ones
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart *
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl
```

### 2.4 Webhook secret

```bash
grep secret /etc/shipit/shipit.yaml
```

- [ ] `secret` is at least 32 random hex characters (the installer generates this automatically)
- [ ] The same value is stored in the `SHIPIT_SECRET` GitHub secret — nowhere else
- [ ] Secret has never appeared in a git commit, log file, or error message

---

## Part 3 — New project checklist (once per project)

Repeat for every project added to `shipit.yaml`.

### 3.1 Project directory

```bash
PROJECT_DIR=/srv/app/beyen-home/web    # adjust per project

stat "$PROJECT_DIR"
stat "$PROJECT_DIR/shared"
```

- [ ] Project directory — owner `shipit:shipit`, mode **`0750`** (not `0755`)
- [ ] `shared/` subdirectory — owner `shipit:shipit`, mode **`0750`**

```bash
# How to set it correctly
sudo mkdir -p "$PROJECT_DIR/shared"
sudo chown -R shipit:shipit "$PROJECT_DIR"
sudo chmod 750 "$PROJECT_DIR"
```

> **Why 0750?** Mode `0755` lets every local user on the machine read your deployed source
> code and traverse into `shared/`. `0750` limits access to the `shipit` group only.

### 3.2 Shared `.env` file

```bash
stat "$PROJECT_DIR/shared/.env"
```

- [ ] `shared/.env` — owner `shipit:shipit`, mode **`0600`**

```bash
# How to create it correctly
sudo -u shipit touch "$PROJECT_DIR/shared/.env"
sudo chmod 600 "$PROJECT_DIR/shared/.env"
sudo -u shipit vi "$PROJECT_DIR/shared/.env"
```

> **Why 0600?** Even with `0750` on the parent directory, group members (e.g. `www-data`) would
> be able to read a `0640` or `0644` `.env`. Mode `0600` makes it owner-only: only the
> `shipit` process and root can read it.

### 3.3 Systemd unit

```bash
systemctl show beyen-home -p LoadState -p User -p WorkingDirectory
```

- [ ] `LoadState=loaded`
- [ ] `User=shipit` — **never empty, never `root`**
- [ ] `WorkingDirectory=<dir>/current`

> `shipit check` already verifies these three fields.
> Additionally, since the last security update, shipit's deploy path runs
> `systemctl show -p User` before every restart and refuses to proceed if the
> unit does not run as the `shipit` user. This is a runtime guard, not just a
> pre-flight warning.

### 3.4 Sudoers entry for this service

```bash
sudo -u shipit sudo -n -l /usr/bin/systemctl restart beyen-home
```

- [ ] Command exits `0` (allowed without password)
- [ ] The rule names `beyen-home` exactly — not a pattern that matches other services

### 3.5 Runtime dependency (project-type specific)

| Type | Server requirement | Verification |
|------|--------------------|-------------|
| `go` | None — static binary | `file releases/v*/myapp` → `ELF 64-bit` |
| `vite` | nginx or similar | `nginx -t` |
| `next` | Node.js runtime | `node --version` |

For `type: next`:

- [ ] Node.js is installed: `which node && node --version`
- [ ] The version satisfies Next.js requirements (check `.nvmrc` or `package.json engines`)
- [ ] `ExecStart` in the unit file uses the full path: `/usr/bin/node server.js`

For `type: vite` served by nginx:

- [ ] nginx's system user (`www-data` on Debian/Ubuntu) is in the `shipit` group:
  ```bash
  groups www-data   # should include shipit
  # If not:
  sudo usermod -aG shipit www-data && sudo systemctl reload nginx
  ```
- [ ] This gives nginx read access to the `0750` project directory while keeping `.env` private

### 3.6 GitHub repository and token

```bash
sudo shipit check
```

- [ ] `shipit check` reports the repo as reachable
- [ ] Token scope is **Contents: read-only** on the target repository only — no broader access
- [ ] For private repos: a per-project token is preferred over the global token

---

## Part 4 — Full permissions reference

| Path | Owner | Mode | Set by |
|------|-------|------|--------|
| `/usr/local/bin/shipit` | `root:root` | `0755` | install.sh |
| `/usr/local/bin/shipit.old` | `root:root` | `0755` | self-update |
| `/etc/systemd/system/shipit.service` | `root:root` | `0644` | install.sh |
| `/etc/systemd/system/<project>.service` | `root:root` | `0644` | admin |
| `/etc/shipit/` | `shipit:shipit` | `0700` | install.sh |
| `/etc/shipit/shipit.yaml` | `shipit:shipit` | `0600` | install.sh |
| `/etc/sudoers.d/shipit` | `root:root` | `0440` | admin |
| `/var/lib/shipit/` | `shipit:shipit` | `0700` | install.sh |
| `/srv/app/<project>/` | `shipit:shipit` | `0750` ⚠ | admin |
| `/srv/app/<project>/releases/` | `shipit:shipit` | `0750` | shipit (auto) |
| `/srv/app/<project>/shared/` | `shipit:shipit` | `0750` ⚠ | admin |
| `/srv/app/<project>/shared/.env` | `shipit:shipit` | `0600` ⚠ | admin |
| `/srv/app/<project>/.shipit.lock` | `shipit:shipit` | `0600` | shipit (auto) |

⚠ = not enforced by shipit; must be set manually.

---

## Part 5 — RCE risk matrix

This table lists every path an attacker could take to execute arbitrary code,
the prerequisite they need, and the control that blocks it.

### Critical — direct path to root

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R1 | Overly permissive sudoers (`systemctl restart *`) → restart a root-owned service → that service executes attacker-controlled code | Access to `shipit` user (webhook or local) | **Exact service name in every sudoers rule** |
| R2 | Unit file has `User=root` or no `User=` → deploy malicious release → restart → root execution | HMAC secret + misconfigured unit | **shipit now checks unit `User=` before every restart** (deploy-time guard); `shipit check` also validates |
| R3 | Supply chain: `beyenpay/shipit` GitHub account compromised → malicious binary in release → `self-update` installs it → runs as root | Compromise of the shipit repository or its CI | Pin `SHIPIT_VERSION`; review binary before upgrading in sensitive environments |

### High — code execution as `shipit` user

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R4 | HMAC secret exposed → attacker calls `POST /v1/deploy` with malicious tag → arbitrary code runs as `shipit` | Secret leak (CI secrets, logs, network sniff) | Rotate secret immediately on exposure; use TLS proxy to hide request contents |
| R5 | `.env` world-readable (`0644`) → attacker reads database password / API key → credential-based access to backend systems | Local user on the same machine, or any process running on the server | **`chmod 600 shared/.env`** |

### Medium — information disclosure or lateral movement

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R6 | Project directory world-readable (`0755`) → local user reads deployed source code → finds hardcoded secrets or exploitable logic | Local user on the same machine | **`chmod 750 /srv/app/<project>`** |
| R7 | One project's running code is compromised → reads `shared/.env` of all other projects (all share the `shipit` user) | Compromise of any single project's code | Understood architectural trade-off: single user = single trust boundary. Mitigated by keeping each `.env` at `0600` |

### Low — contained or authenticated

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R8 | Path traversal in release archive → write files outside project directory | Attacker controls the GitHub release | Strict archive extraction (absolute paths, `..`, symlinks outside dest all rejected) |
| R9 | Replay attack: capture signed request → resubmit | Valid signed request within 5-minute window | Replay cache; timestamps enforce 5-minute window |
| R10 | Rate-limit bypass → brute-force HMAC secret | Direct access to webhook port | Per-IP rate limit; 10 failed auth attempts → 1-minute IP lockout |
| R11 | Health URL as SSRF → probe internal services | Access to `shipit.yaml` config | Health URL is admin-controlled config; probe is a plain GET, no response body is used |

### Fully mitigated (no residual risk)

| Attack | Mitigation |
|--------|------------|
| Command injection via service name | Service names validated by strict regex; `exec` without shell |
| Wrong CPU architecture deployed | ELF header checked against server arch before activation |
| Decompression bomb | 4 GiB unpacked-size limit; 200,000 entry limit |
| Stale signature reuse across endpoints | Signature covers method + path + timestamp + body |
| Deploy while another deploy is running | Per-project `flock(2)` lock |

---

## Part 6 — Quick verification script

Run this after installing shipit or adding a project. Adjust paths as needed.

```bash
#!/usr/bin/env bash
# Quick permission audit for shipit and one project.
# Usage: bash audit.sh /srv/app/beyen-home/web

set -euo pipefail
PROJECT_DIR="${1:-/srv/app/myproject}"
PASS="  ✔"; FAIL="  ✘"

check() {
  local label="$1" expected="$2" actual="$3"
  if [ "$actual" = "$expected" ]; then
    echo "$PASS $label ($actual)"
  else
    echo "$FAIL $label — want $expected, got $actual"
  fi
}

echo "=== shipit installation ==="
check "/usr/local/bin/shipit owner:mode" \
  "root:root 0755" \
  "$(stat -c '%U:%G %a' /usr/local/bin/shipit 2>/dev/null || echo missing)"

check "/etc/shipit/ owner:mode" \
  "shipit:shipit 0700" \
  "$(stat -c '%U:%G %a' /etc/shipit 2>/dev/null || echo missing)"

check "/etc/shipit/shipit.yaml owner:mode" \
  "shipit:shipit 0600" \
  "$(stat -c '%U:%G %a' /etc/shipit/shipit.yaml 2>/dev/null || echo missing)"

check "/etc/sudoers.d/shipit owner:mode" \
  "root:root 0440" \
  "$(stat -c '%U:%G %a' /etc/sudoers.d/shipit 2>/dev/null || echo missing)"

echo ""
echo "=== project: $PROJECT_DIR ==="
check "$PROJECT_DIR owner:mode" \
  "shipit:shipit 0750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR" 2>/dev/null || echo missing)"

check "$PROJECT_DIR/shared owner:mode" \
  "shipit:shipit 0750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR/shared" 2>/dev/null || echo missing)"

if [ -f "$PROJECT_DIR/shared/.env" ]; then
  check "$PROJECT_DIR/shared/.env owner:mode" \
    "shipit:shipit 0600" \
    "$(stat -c '%U:%G %a' "$PROJECT_DIR/shared/.env")"
else
  echo "  - $PROJECT_DIR/shared/.env does not exist yet (create it before first deploy)"
fi

echo ""
echo "=== shipit check (automated) ==="
sudo shipit check
```

---

## Summary

`shipit check` handles the **application-layer** validation (config, units, sudoers rules, GitHub access).
The **filesystem-layer** validation (directory modes, `.env` mode) is not enforced automatically and
must be set during provisioning.

The three items most likely to be missed and most dangerous if wrong:

1. **sudoers rule with a wildcard** → path to root (R1)
2. **`.env` mode `0644` instead of `0600`** → credential leak (R5)
3. **Project directory mode `0755` instead of `0750`** → source code readable by any local user (R6)

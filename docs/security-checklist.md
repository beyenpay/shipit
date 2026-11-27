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
| Systemd unit | Unit is loaded; `User=` is set, exists and is not root (warns if it is `shipit` itself); `WorkingDirectory=<dir>/current` |
| Environment files | Warns for every `EnvironmentFile=` of the unit that the shipit user can read |
| Sudoers rule | `sudo -n systemctl restart <service>` is allowed without a password |

**Gaps — things `shipit check` does not verify:**

- Owner and mode of the project directory (`shipit:svc-<service>`, `2750`), and that the directories above it belong to root
- Owner and mode of the files in `/etc/<service>/` other than the `EnvironmentFile=`s
- `/etc/sudoers.d/shipit` file mode
- Node.js runtime present (required for `type: next`)
- The project directory's group for `type: vite` with nginx
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
ls -ld /srv/app /srv/app/beyen-home    # the directories above it
```

- [ ] Project directory — owner `shipit:svc-<service>`, mode **`2750`** (the `2` is setgid)
- [ ] Every directory above it (`/srv/app`, ...) — owner `root:root`
- [ ] It holds only what shipit manages: `releases/`, `current`, `previous`, `.shipit.lock`

```bash
# How to set it correctly
sudo useradd --system --no-create-home --shell /usr/sbin/nologin svc-beyen-home
sudo mkdir -p "$PROJECT_DIR"
sudo chown shipit:svc-beyen-home "$PROJECT_DIR"
sudo chmod 2750 "$PROJECT_DIR"
```

> **Why `shipit:svc-<service>` and `2750`?**
> shipit writes the releases; the service account reads and runs them through
> the group and cannot change them, so a compromised service cannot leave code
> behind that survives a restart. Nobody else can enter. The setgid bit makes
> every new release inherit the group. Root-owned parents stop shipit from
> renaming or replacing the project directory.

### 3.2 Configuration, data and logs

```bash
sudo stat /etc/beyen-home /etc/beyen-home/.env
sudo -u shipit cat /etc/beyen-home/.env      # must fail: Permission denied
```

- [ ] `/etc/<service>/` — owner `root:svc-<service>`, mode **`0750`**
- [ ] `/etc/<service>/.env` — owner `root:root`, mode **`0600`** (or `root:svc-<service>` `0640` when the program opens the file itself)
- [ ] Other config files (`config.yaml`, ...) — owner `root:svc-<service>`, mode **`0640`**: the service reads them but cannot change them
- [ ] `sudo -u shipit cat /etc/<service>/.env` fails
- [ ] Data in `/var/lib/<service>` (owner = the service account, mode `0700`) and logs in `/var/log/<service>`, created by systemd with `StateDirectory=` and `LogsDirectory=`; no `ReadWritePaths=` for them

```bash
# How to set it correctly
sudo install -d -o root -g svc-beyen-home -m 750 /etc/beyen-home
sudo touch /etc/beyen-home/.env && sudo chmod 600 /etc/beyen-home/.env
sudo vi /etc/beyen-home/.env
```

> **Why `/etc` and `/var` instead of the project directory?**
> Whatever lives inside shipit's territory is inside the reach of anyone who
> compromises shipit. systemd reads `EnvironmentFile=` as root before it starts
> the service, so the secrets never need to be readable by shipit. This
> does **not** hide them from the service itself: a process that has the secrets
> in its environment leaks them when it is compromised. Limit that damage with
> a separate database account and keys per service, and rotate on suspicion.

### 3.3 Systemd unit

```bash
systemctl show beyen-home -p LoadState -p User -p WorkingDirectory -p EnvironmentFiles
```

- [ ] `LoadState=loaded`
- [ ] `User=` is the service's own account — **never empty, never `root`, and not `shipit`**
- [ ] `WorkingDirectory=<dir>/current`
- [ ] `EnvironmentFiles=` point into `/etc/<service>/`
- [ ] `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`

> `shipit check` verifies the first four.
> shipit also checks the unit before every restart, with `systemctl show -p User`,
> and refuses to restart a unit whose `User=` is empty, root (or uid 0) or does
> not exist on the machine. This is a runtime guard, not just a pre-flight warning.
> A unit that still runs as `shipit` is accepted, so projects can be migrated one
> at a time, but `shipit check` warns and `shipit status` marks it with `!`.

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

- [ ] The project directory belongs to nginx's group (`www-data` on Debian/Ubuntu), not to a service group:
  ```bash
  sudo chown shipit:www-data /srv/app/beyen-web
  sudo chmod 2750 /srv/app/beyen-web
  ```
- [ ] No `.env` and no other secret lives in that directory: static sites have none

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
| `/srv/app/` and every directory above the projects | `root:root` | `0755` | admin |
| `/srv/app/<project>/` | `shipit:svc-<service>` | `2750` ⚠ | admin |
| `/srv/app/<project>/releases/` and what is below | `shipit:svc-<service>` | `0755` (group can always read and run) | shipit (auto) |
| `/srv/app/<project>/.shipit.lock` | `shipit:svc-<service>` | `0600` | shipit (auto) |
| `/etc/<service>/` | `root:svc-<service>` | `0750` ⚠ | admin |
| `/etc/<service>/.env` | `root:root` | `0600` ⚠ | admin |
| `/etc/<service>/config.yaml` and similar | `root:svc-<service>` | `0640` ⚠ | admin |
| `/var/lib/<service>/` | `svc-<service>:svc-<service>` | `0700` | systemd (`StateDirectory=`) |
| `/var/log/<service>/` | `svc-<service>:svc-<service>` | `0755` (`0750` with `LogsDirectoryMode=`) | systemd (`LogsDirectory=`) |

⚠ = not enforced by shipit; must be set manually.

---

## Part 5 — RCE risk matrix

This table lists every path an attacker could take to execute arbitrary code,
the prerequisite they need, and the control that blocks it.

### Critical — direct path to root

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R1 | Overly permissive sudoers (`systemctl restart *`) → restart a root-owned service → that service executes attacker-controlled code | Access to `shipit` user (webhook or local) | **Exact service name in every sudoers rule** |
| R2 | Unit file has `User=root` or no `User=` → deploy malicious release → restart → root execution | HMAC secret + misconfigured unit | **shipit checks the unit's `User=` before every restart** and refuses an empty, root, uid-0 or unknown account (deploy-time guard); `shipit check` also validates |
| R3 | Supply chain: `beyenpay/shipit` GitHub account compromised → malicious binary in release → `self-update` installs it → runs as root | Compromise of the shipit repository or its CI | Pin `SHIPIT_VERSION`; review binary before upgrading in sensitive environments |

### High — code execution as `shipit` or as a service account

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R4 | HMAC secret exposed → attacker calls `POST /v1/deploy` with malicious tag → arbitrary code runs as the service account | Secret leak (CI secrets, logs, network sniff) | Rotate secret immediately on exposure; use TLS proxy to hide request contents |
| R5 | `.env` world-readable (`0644`) → attacker reads database password / API key → credential-based access to backend systems | Local user on the same machine, or any process running on the server | **`.env` in `/etc/<service>`, owner `root:root`, mode `0600`** |

### Medium — information disclosure or lateral movement

| # | Attack path | Prerequisite | Control |
|---|------------|-------------|---------|
| R6 | Project directory world-readable (`0755`) → local user reads deployed source code → finds hardcoded secrets or exploitable logic | Local user on the same machine | **project directory `2750`, owned by `shipit:svc-<service>`** |
| R7 | One service's code is compromised → reads the `.env` and the code of the others, or plants code in them | Compromise of any single project's code | **Each service runs as its own account**: it cannot read other services' `/etc`, `/var` or project directories, and cannot write its own code. Residual: services that still run as `shipit` share one trust boundary with it and each other; `shipit status` marks them with `!` and `shipit check` warns |
| R12 | `shipit` (or the webhook) is compromised → attacker plants code in a release and restarts the service → the code runs as that service's account | R4, or a vulnerability in shipit | Cannot read any service's secrets or data (they are outside its reach); its only root capability is the exact restart lines in sudoers. Residual: it can still deploy code to every service it manages, since that is its job. Protect the secret and the release process |

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
# Usage: bash audit.sh /srv/app/beyen-home/web beyen-home

set -euo pipefail
PROJECT_DIR="${1:-/srv/app/myproject}"
SERVICE="${2:-myproject}"
ACCT="svc-$SERVICE"
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
  "root:root 755" \
  "$(stat -c '%U:%G %a' /usr/local/bin/shipit 2>/dev/null || echo missing)"

check "/etc/shipit/ owner:mode" \
  "shipit:shipit 700" \
  "$(stat -c '%U:%G %a' /etc/shipit 2>/dev/null || echo missing)"

check "/etc/shipit/shipit.yaml owner:mode" \
  "shipit:shipit 600" \
  "$(stat -c '%U:%G %a' /etc/shipit/shipit.yaml 2>/dev/null || echo missing)"

check "/etc/sudoers.d/shipit owner:mode" \
  "root:root 440" \
  "$(stat -c '%U:%G %a' /etc/sudoers.d/shipit 2>/dev/null || echo missing)"

echo ""
echo "=== project: $PROJECT_DIR (service $SERVICE, account $ACCT) ==="
check "$PROJECT_DIR owner:mode" \
  "shipit:$ACCT 2750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR" 2>/dev/null || echo missing)"

check "$(dirname "$PROJECT_DIR") owner" \
  "root:root" \
  "$(stat -c '%U:%G' "$(dirname "$PROJECT_DIR")" 2>/dev/null || echo missing)"

check "/etc/$SERVICE owner:mode" \
  "root:$ACCT 750" \
  "$(stat -c '%U:%G %a' "/etc/$SERVICE" 2>/dev/null || echo missing)"

if [ -f "/etc/$SERVICE/.env" ]; then
  case "$(stat -c '%U:%G %a' "/etc/$SERVICE/.env")" in
    "root:root 600" | "root:$ACCT 640") echo "$PASS /etc/$SERVICE/.env owner:mode" ;;
    *) echo "$FAIL /etc/$SERVICE/.env — want root:root 600 (or root:$ACCT 640)" ;;
  esac
  if sudo -u shipit test -r "/etc/$SERVICE/.env"; then
    echo "$FAIL shipit can read /etc/$SERVICE/.env"
  else
    echo "$PASS shipit cannot read /etc/$SERVICE/.env"
  fi
else
  echo "  - /etc/$SERVICE/.env does not exist yet"
fi

check "systemd User=" "$ACCT" "$(systemctl show "$SERVICE" -p User --value 2>/dev/null || echo missing)"

echo ""
echo "=== shipit check (automated) ==="
sudo shipit check
```

---

## Summary

`shipit check` handles the **application-layer** validation (config, units, sudoers rules,
whether shipit can read a service's environment files, GitHub access). The **filesystem-layer**
validation (directory modes, the files in `/etc/<service>`) is not enforced automatically and
must be set during provisioning.

The three items most likely to be missed and most dangerous if wrong:

1. **sudoers rule with a wildcard** → path to root (R1)
2. **a service that still runs as `shipit`, or a `.env` inside the project directory** → one compromised service reaches all of them (R7); look for `!` in `shipit status`
3. **a `.env` readable by others (mode `0644`)** → credential leak (R5)

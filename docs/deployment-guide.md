# Deployment Guide

A step-by-step guide for getting shipit running on a fresh server and adding your first project.

---

## Part 1 — Install shipit on a new server

### 1.1 Prerequisites

- A Linux server (Debian/Ubuntu recommended), amd64 or arm64
- Root access (or a user with `sudo`)
- Outbound HTTPS to GitHub (port 443)
- One inbound TCP port for the webhook (default `7447`)

### 1.2 Run the installer

```bash
curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

The installer:
- Creates the `shipit` system user (no login shell)
- Installs `/usr/local/bin/shipit` (owned by root)
- Writes `/etc/shipit/shipit.yaml` with a randomly-generated secret
- Installs and starts `shipit.service` (runs as `User=shipit`)

To pin a specific version:

```bash
SHIPIT_VERSION=v1.2.3 curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

### 1.3 Verify the installation

```bash
systemctl status shipit          # should be active (running)
sudo shipit check                # validates the full setup
```

`shipit check` without any projects configured will pass all installation checks.

### 1.4 Edit the config

```bash
sudo -u shipit vi /etc/shipit/shipit.yaml
```

Minimal config (projects added in Part 2):

```yaml
listen: :7447
secret: <randomly generated — do not change unless rotating>
```

For private repositories, add a global GitHub token:

```yaml
token: github_pat_...
```

### 1.5 Open the firewall

Allow inbound TCP on the webhook port from GitHub Actions runner IPs,
or from anywhere if you rely on HMAC signing alone:

```bash
# ufw example
sudo ufw allow 7447/tcp
```

GitHub's runner IP ranges are published at
`https://api.github.com/meta` (key: `actions`).

---

## Part 2 — Add a new project

This example deploys a Next.js app called `beyen-home`.
Adjust names, paths, and ports for your project.

### 2.1 Add the project to the config

```bash
sudo -u shipit vi /etc/shipit/shipit.yaml
```

```yaml
projects:
  beyen-home:
    type: next          # go | vite | next
    repo: beyenpay/home
    dir: /srv/app/beyen-home/web
    service: beyen-home
    health: http://127.0.0.1:8012/
```

Config takes effect on the next incoming request — no restart needed.

### 2.2 Create the project directory

```bash
sudo mkdir -p /srv/app/beyen-home/web/shared
sudo chown -R shipit:shipit /srv/app/beyen-home
sudo chmod 750 /srv/app/beyen-home
```

Create the `.env` file before the first deploy so it has the right owner and mode:

```bash
sudo -u shipit touch /srv/app/beyen-home/web/shared/.env
sudo chmod 600 /srv/app/beyen-home/web/shared/.env
sudo -u shipit vi /srv/app/beyen-home/web/shared/.env
```

### 2.3 Create the systemd unit

```bash
sudo vi /etc/systemd/system/beyen-home.service
```

```ini
[Unit]
Description=beyen-home
After=network.target

[Service]
User=shipit
WorkingDirectory=/srv/app/beyen-home/web/current
EnvironmentFile=/srv/app/beyen-home/web/shared/.env
Environment=NODE_ENV=production PORT=8012 HOSTNAME=127.0.0.1
ExecStart=/usr/bin/node server.js
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable it (do **not** start it — `current/` does not exist until the first deploy):

```bash
sudo systemctl daemon-reload && sudo systemctl enable beyen-home
```

### 2.4 Add the sudoers rule

```bash
sudo visudo -f /etc/sudoers.d/shipit
```

Add one exact line per service — no wildcards:

```
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home
```

### 2.5 Verify so far

```bash
sudo shipit check
```

All checks should pass before continuing. Fix any `✘` items.

### 2.6 Add GitHub Secrets to the repository

In the project repository on GitHub → Settings → Secrets and variables → Actions:

| Secret name | Value |
|-------------|-------|
| `SHIPIT_URL` | `http://<server-ip>:7447` |
| `SHIPIT_SECRET` | the `secret` value from `/etc/shipit/shipit.yaml` |

For a private repository, also add:

| Secret name | Value |
|-------------|-------|
| `SHIPIT_TOKEN` | a fine-grained PAT with *Contents: Read-only* on this repo |

And set it in the config:

```yaml
projects:
  beyen-home:
    token: ${{ secrets.SHIPIT_TOKEN }}   # or paste the value directly
```

### 2.7 Add the GitHub Actions workflow

Copy the matching example from
[`examples/workflows/`](../examples/workflows/) to `.github/workflows/release.yml`
in the project repository, and update the variables at the top.

The deploy step in the workflow:

```yaml
- uses: beyenpay/shipit@v1
  with:
    url: ${{ secrets.SHIPIT_URL }}
    secret: ${{ secrets.SHIPIT_SECRET }}
    project: beyen-home
```

### 2.8 Deploy for the first time

On your local machine, in the project repository:

```bash
git tag v1.0.0
git push origin v1.0.0
```

What happens next:

1. GitHub Actions triggers on the new tag
2. The workflow builds the release artifact and publishes it to GitHub Releases
3. The workflow calls `POST /v1/deploy` on your server (HMAC-signed)
4. shipit downloads the release, verifies the checksum, unpacks it
5. shipit checks the binary/package type and architecture
6. `current` symlink is switched to the new release
7. `sudo systemctl restart beyen-home` is issued
8. The health URL is polled until it returns 2xx (or the timeout is reached)
9. If the health check fails, shipit switches back to `previous` and restarts
10. The CI job turns green or red with the real outcome

Watch the deploy live:

```bash
journalctl -u shipit -f          # shipit's own log
journalctl -u beyen-home -f      # the deployed app's log
```

### 2.9 Subsequent deploys

Every new tag triggers a full deploy:

```bash
git tag v1.0.1
git push origin v1.0.1
```

Manual deploy from the server (useful for testing):

```bash
sudo shipit deploy beyen-home            # latest release on GitHub
sudo shipit deploy beyen-home v1.0.0     # specific version
sudo shipit rollback beyen-home          # back to previous
```

---

## Checklist

Run after completing both parts. Items marked [auto] are verified by `shipit check`.

### shipit installation

- [ ] `systemctl status shipit` — active (running)
- [ ] `sudo shipit check` — all green [auto]
- [ ] `getent passwd shipit | cut -d: -f7` — shell is `/usr/sbin/nologin` or `/bin/false`
- [ ] `sudo stat /etc/shipit/` — mode `0700`, owner `shipit:shipit`
- [ ] `sudo stat /etc/shipit/shipit.yaml` — mode `0600`, owner `shipit:shipit`
- [ ] `stat /etc/sudoers.d/shipit` — mode `0440`, owner `root:root`
- [ ] sudoers contains **exact** service names, no wildcards

### Per project

- [ ] `stat /srv/app/<project>` — mode `0750`, owner `shipit:shipit`
- [ ] `sudo stat /srv/app/<project>/shared/.env` — mode `0600`, owner `shipit:shipit`
- [ ] `systemctl show <service> -p User` — `User=shipit`
- [ ] `sudo shipit check` — unit, sudoers and repo all pass [auto]
- [ ] GitHub Secrets `SHIPIT_URL` and `SHIPIT_SECRET` are set in the repository
- [ ] First deploy succeeded: `sudo shipit status`

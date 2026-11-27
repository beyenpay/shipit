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

### 2.2 Create the service account and directories

shipit deploys; the service runs as an account of its own, so that a compromised
service cannot reach the others. Each kind of file has one place and one owner:

| path | holds | owner : mode |
|---|---|---|
| `/srv/app/beyen-home/web` | `releases/`, `current`, `previous`: everything shipit touches, nothing else | `shipit:svc-beyen-home` `2750` |
| `/etc/beyen-home` | `.env` and config files | `root:svc-beyen-home` `0750` |
| `/etc/beyen-home/.env` | the secrets | `root:root` `0600` |
| `/var/lib/beyen-home` | data the program keeps | created by systemd (`StateDirectory=`), owned by the service account |
| `/var/log/beyen-home` | log files | created by systemd (`LogsDirectory=`), owned by the service account |

```bash
# the service account: no login, no home
sudo useradd --system --no-create-home --shell /usr/sbin/nologin svc-beyen-home
id svc-beyen-home                      # its group of the same name must exist

# the project directory: shipit writes, the service only reads, nobody else enters
sudo mkdir -p /srv/app/beyen-home/web
sudo chown shipit:svc-beyen-home /srv/app/beyen-home/web
sudo chmod 2750 /srv/app/beyen-home/web      # setgid: each new release inherits the group
ls -ld /srv/app /srv/app/beyen-home          # must be owned by root, so shipit cannot swap the directory

# configuration: root's, the service can read it but not change it
sudo install -d -o root -g svc-beyen-home -m 750 /etc/beyen-home
sudo touch /etc/beyen-home/.env && sudo chmod 600 /etc/beyen-home/.env
sudo vi /etc/beyen-home/.env
```

Why it is built this way:

- shipit can write the code but the service cannot, so a compromised service
  cannot plant code that survives its next restart.
- shipit never sees the secrets. systemd reads `EnvironmentFile=` as root before it
  starts the service, so `.env` can be `root:root 0600`. If the program opens the
  file itself instead of reading environment variables, make it `root:svc-beyen-home` `0640`.
- A service that is compromised still has its own secrets (the process needs them),
  but not those of the other services. Give every service its own database
  account and keys, and rotate them if you suspect a compromise.
- The program must write only to `/var/lib/beyen-home` and `/var/log/beyen-home`, or
  log to stdout (journald collects it). Anything it writes into its own
  directory, such as `./data` or `./log`, fails because that directory is read-only to it.

For a `vite` project there is no service, hence no account: give the directory to
nginx's group instead, `sudo chown shipit:www-data <dir> && sudo chmod 2750 <dir>`
(`www-data` on Debian/Ubuntu).

### 2.3 Create the systemd unit

```bash
sudo vi /etc/systemd/system/beyen-home.service
```

```ini
[Unit]
Description=beyen-home
After=network.target

[Service]
User=svc-beyen-home
Group=svc-beyen-home
WorkingDirectory=/srv/app/beyen-home/web/current
EnvironmentFile=/etc/beyen-home/.env
Environment=NODE_ENV=production PORT=8012 HOSTNAME=127.0.0.1
ExecStart=/usr/bin/node server.js
Restart=always
RestartSec=2

# systemd creates these and gives them to the service account
StateDirectory=beyen-home
StateDirectoryMode=0700
LogsDirectory=beyen-home

# Sandbox: everything is read-only except the two directories above and /tmp
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes

[Install]
WantedBy=multi-user.target
```

`User=` must be set and must not be root: shipit refuses to restart such a unit,
and `shipit check` fails it. Do not share the account between services. Do not
use `ReadWritePaths=` for the directories above; `StateDirectory=` and
`LogsDirectory=` already make them writable, and `ReadWritePaths=` on a path that
does not exist stops the service from starting.

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

All checks should pass before continuing. Fix any `✘` items. A `!` is a warning
that does not stop a deploy, but is worth acting on; in particular:

- `env file ... can be read by shipit`: the secrets are inside shipit's reach.
  Move the file to `/etc/<service>/` and make it owned by root.
- `... runs as shipit, like shipit and every other project that does`: the service
  still shares an account with shipit (see [Migrating an existing project](#migrating-an-existing-project)).

`sudo shipit status` shows the account each service runs as, in the `RUNNER`
column. `shipit` and `root` are marked with `!`.

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

## Migrating an existing project

Projects set up the old way (running as `shipit`, with `shared/.env` inside the
project directory) keep working. Move them one at a time, the least important first.

Upgrade shipit first, since older versions refuse a unit that does not run as
`shipit`: `sudo shipit self-update`. Then, for a service `beyen-api` in
`/srv/app/beyen-api`:

```bash
SVC=beyen-api; DIR=/srv/app/beyen-api; ACCT=svc-$SVC

sudo cp -a "$DIR" /root/$SVC.bak                     # a way back
sudo useradd --system --no-create-home --shell /usr/sbin/nologin $ACCT
id $ACCT
sudo systemctl stop $SVC                             # downtime starts here

# configuration -> /etc, owned by root
sudo install -d -o root -g $ACCT -m 750 /etc/$SVC
sudo mv "$DIR/shared/.env" /etc/$SVC/.env
sudo chown root:root /etc/$SVC/.env && sudo chmod 600 /etc/$SVC/.env
# other config files: sudo mv "$DIR/shared/config.yaml" /etc/$SVC/ and
#   sudo chown root:$ACCT /etc/$SVC/config.yaml && sudo chmod 640 /etc/$SVC/config.yaml

# data and logs -> /var, owned by the service account
sudo install -d -o $ACCT -g $ACCT -m 700 /var/lib/$SVC
sudo install -d -o $ACCT -g $ACCT -m 750 /var/log/$SVC
sudo sh -c "mv '$DIR'/shared/data/* /var/lib/$SVC/"         # adjust to what your app keeps
sudo sh -c "mv '$DIR'/shared/logs/* /var/log/$SVC/"
sudo chown -R $ACCT:$ACCT /var/lib/$SVC /var/log/$SVC

# project directory: shipit writes, the service reads
sudo chgrp -R $ACCT "$DIR"
sudo chmod -R g+rX "$DIR"
sudo find "$DIR" -type d -exec chmod g+s {} +        # releases/ must inherit the group too
sudo chmod 2750 "$DIR"
sudo chown root:root /srv/app                        # and every directory above $DIR
```

Then change the unit (`sudo systemctl edit --full $SVC`), following section 2.3:

| old | new |
|---|---|
| `User=shipit`, `Group=shipit` | `User=svc-beyen-api`, `Group=svc-beyen-api` |
| `EnvironmentFile=<dir>/shared/.env` | `EnvironmentFile=/etc/beyen-api/.env` |
| `ReadWritePaths=<dir>/shared/...` | remove; add `StateDirectory=beyen-api` and `LogsDirectory=beyen-api` |

Point the application at the new locations: any `LOG_DIR`, `DATA_DIR` or config
path in the `.env`, or in the code if it is hard-coded, must now be
`/var/log/beyen-api`, `/var/lib/beyen-api` and `/etc/beyen-api/...`. Relative
paths inside `current/` fail, because that directory is read-only to the service.

```bash
sudo systemctl daemon-reload
sudo shipit check                                    # no ✘; no env or shared-account warnings
sudo systemctl start $SVC
```

Verify before you delete anything:

```bash
sudo shipit status                                   # RUNNER column shows svc-beyen-api, no "!"
sudo journalctl -u $SVC -n 50
sudo -u shipit cat /etc/$SVC/.env                    # must fail with "Permission denied"
```

Deploy a release (or `sudo shipit deploy $SVC`) once to prove that a new release is
readable by the service. If anything is wrong, stop the service, put `/root/$SVC.bak`
back and restore the old unit. When it has run well for a few days, delete the
backup (`/root/$SVC.bak` still holds the old `.env`) and the empty `$DIR/shared`.

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

- [ ] `stat /srv/app/<project>` — mode `2750`, owner `shipit:svc-<service>`; the directories above it are owned by `root`
- [ ] `sudo stat /etc/<service>/.env` — mode `0600`, owner `root:root` (or `root:svc-<service>` `0640` if the program reads the file itself)
- [ ] `sudo -u shipit cat /etc/<service>/.env` — fails with *Permission denied*
- [ ] `systemctl show <service> -p User` — the service's own non-root account, not `shipit`
- [ ] `sudo shipit check` — unit, sudoers, repo pass, and no warning about readable env files [auto]
- [ ] `sudo shipit status` — the `RUNNER` column shows the service account, without `!`
- [ ] GitHub Secrets `SHIPIT_URL` and `SHIPIT_SECRET` are set in the repository
- [ ] First deploy succeeded: `sudo shipit status`

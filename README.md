# shipit

Pull-based deploys from GitHub Releases for Go, Vite and Next.js projects on a
plain Linux server. One static binary, one config file, one user, no Docker.

Push a tag. CI builds and publishes a release, calls the shipit webhook on your
server, and waits. shipit downloads the release, verifies it, switches a
symlink, restarts the service, checks that it is healthy, and rolls back by
itself if it is not. The CI job turns green or red with the real outcome.

## Features

- **Tag-driven**: `git tag v1.2.0 && git push --tags` is the whole release process.
- **Pull model**: the server downloads from GitHub. CI holds no SSH keys and no server credentials, only a webhook secret.
- **Atomic and rollback-able**: releases live side by side; `current` and `previous` are symlinks. Rolling back is a symlink switch, not a rebuild.
- **Health-checked**: an optional health URL is polled after the restart; a failing release is replaced by the previous one automatically.
- **Public and private repositories**: a GitHub token is only needed for private ones.
- **Both architectures**: releases carry `amd64` and `arm64` packages; each server picks its own.
- **Minimal privileges**: one `shipit` user, and sudo for exactly `systemctl restart <service>`, nothing else.
- **Small surface**: one YAML file. Systemd units, sudoers and nginx stay yours, in the usual places.

## How it works

```
git push tag v1.2.0
   │
   ▼
GitHub Actions ── build ── publish release (packages + checksums.txt)
   │
   └── POST /v1/deploy {project, tag}   (HMAC-signed)
          │
          ▼  your server: shipit serve
       download → sha256 check → unpack → type checks → current → restart → health check
          │                                                              └─ failed? back to previous
          ▼
   CI polls /v1/jobs/<id> and shows the log; success or failure ends the job
```

On disk, per project:

```
/srv/app/beyen-home/web/
  releases/v1.1.0/  releases/v1.2.0/
  current  -> releases/v1.2.0       what is live
  previous -> releases/v1.1.0       what `rollback` returns to
  shared/.env                       yours; shipit never touches it
```

The two symlinks are the only state shipit keeps. There is no database and no
state file that could disagree with reality.

## Project types

The `type` in the config decides what shipit expects in a release, and
rejects a wrong package before it goes live.

| type   | package name                              | must contain                                           | `service` |
|--------|-------------------------------------------|--------------------------------------------------------|-----------|
| `go`   | `<asset>-<tag>-linux-<arch>.tar.gz`       | executable `<asset>` in the root, built for the server's CPU | required |
| `next` | `<asset>-<tag>-linux-<arch>.tar.gz`       | `server.js` and `.next/static/` (standalone output)    | required |
| `vite` | `<asset>-<tag>-any.tar.gz`                | `index.html`                                           | not allowed |

`<arch>` is `amd64` or `arm64`. `<asset>` defaults to the project name. Every
release also needs a `checksums.txt` (the output of `sha256sum`) listing each
package. Archives are unpacked as-is, so build them with `tar -czf x.tar.gz -C <dir> .`.

Publish **both** `linux-amd64` and `linux-arm64` for `go` and `next`: moving a
project to another server then needs no rebuild.

## Install

On the server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

This installs `/usr/local/bin/shipit`, creates the `shipit` user, writes
`/etc/shipit/shipit.yaml` with a random secret, and starts `shipit.service`.
Running it again upgrades the binary and keeps your config. Pin a version with
`SHIPIT_VERSION=v1.2.3`.

Then:

1. Edit the config: `sudo -u shipit vi /etc/shipit/shipit.yaml`
2. Open the webhook port (default `9000/tcp`) in your firewall.
3. Add your projects (below).
4. Run `sudo -u shipit shipit check`.

Handy: `alias shipit='sudo -u shipit shipit'`

## Upgrade shipit

On the server, as root:

```bash
sudo shipit self-update                    # to the latest release
sudo shipit self-update -version v1.2.3    # to a specific one (also works as a downgrade)
sudo shipit self-update -dry-run           # only show what would happen
```

It downloads the binary for the server's CPU, verifies it against
`checksums.txt`, and runs it once (`shipit version`) before touching anything.
Then the binary is swapped in atomically, `shipit.service` is restarted (a deploy
in progress finishes first) and checked after a few seconds. If the service does
not come back, the previous binary is restored automatically. The old binary
stays next to the new one as `/usr/local/bin/shipit.old`.

Use plain `sudo shipit ...`, not the `sudo -u shipit` alias: the binary belongs
to root, and the webhook deliberately cannot trigger an update. Re-running
`install.sh` also upgrades, and is the way to get `self-update` the first time
on a version that predates it.

## Add a project

Example: a Next.js site called `beyen-home`. Samples for every step are in [`examples/`](examples).

**1. Config** (`/etc/shipit/shipit.yaml`; takes effect on the next request):

```yaml
projects:
  beyen-home:
    type: next
    repo: beyenpay/home
    dir: /srv/app/beyen-home/web
    service: beyen-home
    health: http://127.0.0.1:8012/
```

**2. Directory:**

```bash
sudo mkdir -p /srv/app/beyen-home/web/shared
sudo chown -R shipit:shipit /srv/app/beyen-home
sudo -u shipit vi /srv/app/beyen-home/web/shared/.env      # optional
```

**3. Systemd unit** (`/etc/systemd/system/beyen-home.service`): see
[`examples/systemd`](examples/systemd). It must run as `User=shipit` and use
`WorkingDirectory=<dir>/current`. Enable it, but do not start it yet:

```bash
sudo systemctl daemon-reload && sudo systemctl enable beyen-home
```

**4. Sudoers**, one exact line per service (`sudo visudo -f /etc/sudoers.d/shipit`):

```
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home
```

**5. Verify:** `sudo -u shipit shipit check` tells you what is still missing
(unit not found, wrong user, wrong working directory, sudoers rule absent,
repository not reachable, token expiring).

Vite projects need only steps 1 and 2: point nginx at `<dir>/current`. The
switch is atomic, no reload needed.

## Connect GitHub

In each project repository add two secrets: `SHIPIT_URL` (for example
`http://203.0.113.10:9000`) and `SHIPIT_SECRET` (the `secret` from the config).
Then copy a workflow from [`examples/workflows`](examples/workflows)
(`go.yml`, `vite.yml` or `next.yml`) to `.github/workflows/release.yml` and
change the two or three values at its top.

The deploy step is a single action:

```yaml
- uses: beyenpay/shipit@v1
  with:
    url: ${{ secrets.SHIPIT_URL }}
    secret: ${{ secrets.SHIPIT_SECRET }}
    project: beyen-home
```

For a **private repository** also create a fine-grained personal access token
(Repository access: the repos to deploy; Permissions: *Contents: Read-only*)
and put it in `token:` in the config, globally or per project. Check its
expiry with `shipit check`.

## Everyday use

```bash
git tag v1.2.0 && git push --tags      # build, release, deploy
```

On the server:

```bash
shipit status                  # current / previous version and service state of every project
shipit list beyen-home         # releases on disk
shipit deploy beyen-home       # deploy the latest GitHub release
shipit deploy beyen-home v1.2.0  # a specific one (no download if it is already on disk)
shipit rollback beyen-home     # back to the previous version
shipit rollback beyen-home v1.0.0  # back to a version on disk, never downloads
shipit check [-offline]        # validate the whole setup
```

`-c <file>` (before the command) or `$SHIPIT_CONFIG` selects another config file.

Notes:

- Two `rollback`s in a row switch back and forth between the same two versions.
- A rollback to an old version is a normal switch: it is restarted and health-checked too.
- Only `keep` releases (default 5) stay on disk; the current and previous ones are never removed.
- If a version was re-published under the same tag, delete `releases/<tag>` on the server before deploying it again.
- Rollbacks revert code, not data. Keep database migrations backward compatible.

You can also roll back without logging in: add
[`examples/workflows/rollback.yml`](examples/workflows/rollback.yml) and run it
from the Actions tab.

## Configuration reference

`/etc/shipit/shipit.yaml`, mode `0600`, owned by `shipit`. Unknown keys are errors.

| key | | |
|---|---|---|
| `listen` | optional | address of the webhook, default `:9000` |
| `secret` | required to serve | shared with CI, at least 16 characters |
| `token` | optional | GitHub token, default for all projects |
| `projects.<name>.type` | required | `go`, `vite` or `next` |
| `projects.<name>.repo` | required | `owner/name` |
| `projects.<name>.dir` | required | absolute project directory |
| `projects.<name>.service` | go/next: required; vite: forbidden | systemd unit restarted after the switch |
| `projects.<name>.health` | optional | http(s) URL that must answer 2xx after a restart |
| `projects.<name>.health_timeout` | optional | default `30s`, at most `10m` |
| `projects.<name>.asset` | optional | package name prefix, default = project name |
| `projects.<name>.keep` | optional | releases kept on disk, default `5`, minimum `2` |
| `projects.<name>.token` | optional | overrides the global token |

Project directories must not overlap and services must not be shared.

## Webhook API

All endpoints except `/healthz` need a signature. Bodies are JSON.

| | |
|---|---|
| `POST /v1/deploy` | `{"project": "...", "tag": "v1.2.0"}` (tag required) |
| `POST /v1/rollback` | `{"project": "...", "tag": "v1.1.0"}` (tag optional = previous) |
| `GET /v1/jobs/<id>` | job status and log |
| `GET /healthz` | liveness, unauthenticated |

By default a `POST` returns `202` with a `job_id`; poll the job until `status`
is `success` or `failed`. With `?wait=true` the response is held open and
carries the final result (`200`, `500` on failure, `409` if the project is
busy). The `action` and `scripts/call.sh` do the polling for you.

Signature: send `X-Shipit-Timestamp` (unix seconds) and `X-Shipit-Signature`:

```
hex( HMAC-SHA256( secret, timestamp + "\n" + METHOD + "\n" + path + "\n" + body ) )
```

```bash
ts=$(date +%s); body='{"project":"beyen-home","tag":"v1.2.0"}'
sig=$(printf '%s\n%s\n%s\n%s' "$ts" POST /v1/deploy "$body" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')
curl -X POST "$URL/v1/deploy" -H "X-Shipit-Timestamp: $ts" -H "X-Shipit-Signature: $sig" -d "$body"
```

Jobs are kept in memory for an hour. If shipit restarts, a job is gone; the
state of the project is always visible with `shipit status`.

## Security model

- **The webhook cannot run commands.** It accepts a project name and a tag, validates both, and only ever *deploys a release that exists in that project's own GitHub repository*. Commands, URLs and paths come from the config file, never from a request.
- **Signed requests**: HMAC over the method, path, timestamp and body. Timestamps must be within 5 minutes, and a signature is accepted once, so a captured request cannot be replayed or redirected to another endpoint. The secret is not sent over the wire, which matters because the webhook is plain HTTP by default. Put a TLS proxy in front, or set up a firewall rule for GitHub's runners, if you want to hide request contents too.
- **Rate limits**, per client IP: 10 requests/s (burst 20), and an IP with 10 failed authentications in a minute is locked out for the rest of that minute. A global limit would let anyone lock CI out, so it is per IP.
- **Release integrity**: every package is checked against `checksums.txt`; archives are unpacked with strict path and symlink rules, size limits, and no overwriting. This protects against corruption, not against someone who can publish releases to your repository: that access is the real security boundary.
- **One user** runs shipit and the projects. Systemd units must say `User=shipit`; `shipit check` fails a unit that would run as root. The only root capability is the exact `sudo systemctl restart <service>` lines you add.
- **Secrets**: the config (secret and tokens) must be mode `0600`; shipit refuses to serve otherwise.
- The webhook shuts down gracefully: a deploy in progress finishes before the process exits.

Because a single user owns both the webhook and the projects, compromising
shipit means compromising the deployed apps. The controls above exist to make
that hard, not to separate the two.

## Uninstall

```bash
sudo shipit uninstall                  # removes the binary and shipit.service; keeps config and projects
sudo shipit uninstall -purge           # also removes /etc/shipit, optionally the shipit user
sudo shipit uninstall -purge -delete-projects   # also deletes the project directories (asks twice)
```

Add `-dry-run` to see what would happen and `-y` to skip the questions. shipit
never removes files it did not create, such as `/etc/sudoers.d/shipit` or your
projects' systemd units; it lists them so you can clean up. Project directories
are only deleted when they look like shipit projects (a real directory with a
`releases/` folder).

## Troubleshooting

| symptom | cause |
|---|---|
| CI: `rejected: bad signature or clock skew` | wrong `SHIPIT_SECRET`, or the server clock is more than 5 minutes off (enable NTP) |
| CI: `429` | rate limit, or too many failed attempts from that IP; wait a minute |
| `404 ... private repos need a token` | wrong tag/repo, or the token cannot read the repository |
| `release has no asset ...` | the package name does not match the table above; the error lists what the release contains |
| `built for EM_AARCH64 but this server is amd64` | wrong architecture packaged |
| `missing .next/static` | the Next.js package was built without copying `.next/static` |
| `sudo: a password is required` | the sudoers line is missing or does not match exactly |
| `... rolled back to vX` | the new version failed its restart or health check; look at `journalctl -u <service>` |
| `another operation is in progress` | a deploy for that project is already running |

Logs: `journalctl -u shipit -f`.

## Releasing shipit itself

Push a tag `vX.Y.Z`. The workflow builds `shipit-linux-amd64`, `shipit-linux-arm64` and
`checksums.txt`, publishes the release and moves the major tag (`v1`) used by
`uses: beyenpay/shipit@v1`.

## License

MIT

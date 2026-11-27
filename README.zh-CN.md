# shipit

[English](README.md)

从 GitHub Releases 拉取并部署 Go、Vite 和 Next.js 项目，运行在普通 Linux 服务器上。
一个静态二进制文件、一个配置文件，无需 Docker。

推送一个 tag。CI 构建并发布 Release，调用服务器上的 shipit webhook，然后等待。
shipit 下载 Release、校验完整性、切换符号链接、重启服务、检查健康状态，
若新版本不正常则自动回滚。CI 任务最终显示真实的成功或失败。

## 功能特性

- **Tag 驱动**：`git tag v1.2.0 && git push --tags` 即为完整的发布流程。
- **拉取模型**：服务器主动从 GitHub 下载。CI 不持有任何 SSH 密钥和服务器凭证，只有一个 webhook secret。
- **原子部署 + 可回滚**：Release 并排保存；`current` 和 `previous` 是符号链接。回滚只需切换符号链接，无需重新构建。
- **健康检查**：重启后可选轮询一个健康 URL；若新版本不健康，自动恢复到前一版本。
- **支持公开和私有仓库**：私有仓库才需要 GitHub token。
- **双架构支持**：Release 包含 `amd64` 和 `arm64` 两种包；每台服务器自动选择对应架构。
- **最小权限**：shipit 只负责部署。每个服务以自己独立的账号运行，shipit 读不到它们的密钥和数据；shipit 只有对 `systemctl restart <service>` 精确的 sudo 权限，仅此而已。
- **配置面最小**：只有一个 YAML 文件。systemd unit、sudoers 和 nginx 仍由你管理，放在各自的常规位置。

## 工作原理

```
git push tag v1.2.0
   │
   ▼
GitHub Actions ── 构建 ── 发布 Release（包 + checksums.txt）
   │
   └── POST /v1/deploy {project, tag}   （HMAC 签名）
          │
          ▼  你的服务器：shipit serve
       下载 → sha256 校验 → 解包 → 类型检查 → 切换 current → 重启 → 健康检查
          │                                                        └─ 失败？回到 previous
          ▼
   CI 轮询 /v1/jobs/<id> 并输出日志；成功或失败结束任务
```

磁盘上，每个项目的目录结构：

```
/srv/app/beyen-home/web/
  releases/v1.1.0/  releases/v1.2.0/
  current  -> releases/v1.2.0       当前线上版本
  previous -> releases/v1.1.0       `rollback` 回到的版本
```

这里不放别的东西。配置放在 `/etc/<service>`，数据和日志放在 `/var/lib/<service>` 和
`/var/log/<service>`（见[添加项目](#添加项目)）。

这两个符号链接是 shipit 保存的全部状态，没有数据库，没有可能与现实不一致的状态文件。

## 项目类型

配置中的 `type` 决定 shipit 对 Release 包的预期，并在上线前拒绝格式不符的包。

| type   | 包名                                      | 必须包含                                                   | `service` |
|--------|-------------------------------------------|------------------------------------------------------------|-----------|
| `go`   | `<asset>-<tag>-linux-<arch>.tar.gz`       | 根目录下可执行文件 `<asset>`，且已为服务器 CPU 架构编译     | 必填      |
| `next` | `<asset>-<tag>-linux-<arch>.tar.gz`       | `server.js` 和 `.next/static/`（standalone 输出）           | 必填      |
| `vite` | `<asset>-<tag>-any.tar.gz`                | `index.html`                                               | 禁止填写  |

`<arch>` 为 `amd64` 或 `arm64`。`<asset>` 默认为项目名。每个 Release 还需要一个
`checksums.txt`（`sha256sum` 的输出），列出每个包的校验和。
打包时使用 `tar -czf x.tar.gz -C <dir> .`，解包后文件直接位于根目录。

`go` 和 `next` 建议同时发布 `linux-amd64` 和 `linux-arm64`，
这样迁移服务器时无需重新构建。

## 安装

在服务器上，以 root 身份执行：

```bash
curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

此命令会安装 `/usr/local/bin/shipit`、创建 `shipit` 用户、写入含随机 secret 的
`/etc/shipit/shipit.yaml`，并启动 `shipit.service`。
再次运行可升级二进制并保留现有配置。
用 `SHIPIT_VERSION=v1.2.3` 可固定版本。

然后：

1. 编辑配置：`sudo -u shipit vi /etc/shipit/shipit.yaml`
2. 在防火墙中开放 webhook 端口（默认 `7447/tcp`）。
3. 添加项目（见下文）。
4. 运行 `sudo shipit check`。

### 谁在运行什么

你不需要 `sudo -u shipit` 也不需要 shell 别名。以 root（或 `sudo`）运行 shipit，
它会自动切换到正确的账号：

| 命令 | 以哪个用户运行 | 若你是 root | 若你是其他用户 |
|---|---|---|---|
| `deploy` `rollback` `list` `status` `check` | `shipit` | 自动切换到 `shipit` | 提示使用 `sudo shipit ...` |
| `self-update` `uninstall` | `root` | 直接运行 | 提示使用 `sudo shipit ...` |
| `version` | 任意 | | |

这个切换很重要：若 root 运行的 deploy 在项目目录创建了文件，这些文件将属于 root，
导致后续 webhook 触发的 deploy 失败。因此 shipit 从不以 root 身份运行这些命令，
`check` 也验证的是 `shipit` 用户实际拥有的权限，而非 root 拥有的权限。

## 升级 shipit

在服务器上，以 root 身份执行：

```bash
sudo shipit self-update                    # 升级到最新版本
sudo shipit self-update -version v1.2.3    # 升级/降级到指定版本
sudo shipit self-update -dry-run           # 仅预览，不实际操作
```

它会下载对应服务器 CPU 架构的二进制，对照 `checksums.txt` 校验，
并在替换前运行一次 `shipit version`。
然后原子替换二进制，重启 `shipit.service`（正在进行的 deploy 会先完成），
几秒后检查服务是否正常。若服务未恢复，自动还原旧二进制。
旧二进制保留为 `/usr/local/bin/shipit.old`。

webhook 故意不能触发更新：二进制属于 root，更新需要 root 权限。
重新运行 `install.sh` 也可以升级，这也是在旧版本上首次获得 `self-update` 的方式。

> **供应链信任**：`self-update` 和 `install.sh` 会将下载的二进制与同一 GitHub Release
> 的 `checksums.txt` 进行校验。这可以防止网络中间人攻击，但无法防范已被攻破的 Release。
> 请保护 `beyenpay/shipit` 仓库的 tag 和 Release 权限。
> 在安全敏感的环境中，建议固定 `SHIPIT_VERSION` 并在升级前人工审查二进制。

## 添加项目

示例：一个叫 `beyen-home` 的 Next.js 站点。每个步骤的示例文件见 [`examples/`](examples)。

**1. 配置**（`/etc/shipit/shipit.yaml`；下次请求时生效）：

```yaml
projects:
  beyen-home:
    type: next
    repo: beyenpay/home
    dir: /srv/app/beyen-home/web
    service: beyen-home
    health: http://127.0.0.1:8012/
```

**2. 账号与目录。** shipit 负责部署，服务以自己独立的账号运行。每类文件只有一个位置、一个属主：

| 路径 | 内容 | 属主 : 权限 |
|---|---|---|
| `/srv/app/beyen-home/web` | `releases/`、`current`、`previous`：shipit 只碰这些 | `shipit:svc-beyen-home` `2750` |
| `/etc/beyen-home` | `.env`、配置文件 | `root:svc-beyen-home` `0750` |
| `/etc/beyen-home/.env` | 密钥 | `root:root` `0600` |
| `/var/lib/beyen-home`、`/var/log/beyen-home` | 数据和日志 | 由 systemd 创建，属于服务账号 |

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin svc-beyen-home
sudo mkdir -p /srv/app/beyen-home/web
sudo chown shipit:svc-beyen-home /srv/app/beyen-home/web
sudo chmod 2750 /srv/app/beyen-home/web       # setgid：每个新版本都继承该组
sudo install -d -o root -g svc-beyen-home -m 750 /etc/beyen-home
sudo touch /etc/beyen-home/.env && sudo chmod 600 /etc/beyen-home/.env
sudo vi /etc/beyen-home/.env
```

原因：shipit 能写代码，服务只能读，所以被攻破的服务无法植入重启后仍然存在的代码；
服务看不到其他服务；shipit 也看不到任何服务的密钥，因为 systemd 在启动服务之前以
root 身份读取 `EnvironmentFile=`。如果程序是自己打开 `.env` 文件而不是读环境变量，
把该文件设为 `root:svc-beyen-home` `0640`。`dir` 的上层目录（`/srv/app`、
`/srv/app/beyen-home`）应归 root 所有，这样 shipit 无法替换项目目录。

程序只应写入 `/var/lib/beyen-home` 和 `/var/log/beyen-home`（或把日志输出到 stdout，
由 journald 收集），不要写入自己的目录。

**3. Systemd unit**（`/etc/systemd/system/beyen-home.service`）：
参见 [`examples/systemd`](examples/systemd)。
需要 `User=svc-beyen-home`（任何不与其他服务共用的非 root 账号）、
`WorkingDirectory=<dir>/current`、`EnvironmentFile=/etc/beyen-home/.env`，
以及 `StateDirectory=`/`LogsDirectory=`，让 systemd 创建数据和日志目录。
启用但暂不启动：

```bash
sudo systemctl daemon-reload && sudo systemctl enable beyen-home
```

**4. Sudoers**，每个服务一行（`sudo visudo -f /etc/sudoers.d/shipit`）：

```
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home
```

**5. 验证：** `sudo shipit check` 会告诉你还缺什么
（unit 未找到、以 root 运行、工作目录错误、sudoers 规则缺失、仓库不可访问、token 即将过期），
并对 shipit 用户能读到的 `EnvironmentFile=`、以及仍以 `shipit` 运行的服务给出警告。
`shipit status` 会显示每个服务以哪个账号运行。

仍以 `shipit` 运行、并把 `shared/.env` 放在项目目录里的旧项目可以继续工作，
但它们与 shipit 以及彼此之间共用一个信任边界。`shipit status` 会用 `!` 标出它们。
迁移方法见 [docs/deployment-guide.zh-CN.md](docs/deployment-guide.zh-CN.md#迁移已有项目)。

Vite 项目只需第 1、2 步，且不需要服务账号：将 nginx 指向 `<dir>/current`，
切换是原子的，无需 reload。把目录的属组设为 nginx 所在的组，它就能读取文件：

```bash
sudo mkdir -p /srv/app/beyen-web
sudo chown shipit:www-data /srv/app/beyen-web   # Debian/Ubuntu 上是 www-data；其他发行版请调整
sudo chmod 2750 /srv/app/beyen-web
```

## 连接 GitHub

在每个项目仓库中添加两个 Secret：
`SHIPIT_URL`（例如 `http://203.0.113.10:7447`）和 `SHIPIT_SECRET`（配置文件中的 `secret`）。
然后从 [`examples/workflows`](examples/workflows) 复制对应的 workflow 文件
（`go.yml`、`vite.yml` 或 `next.yml`）到 `.github/workflows/release.yml`，
修改顶部的两三个变量即可。

deploy 步骤只需一个 action：

```yaml
- uses: beyenpay/shipit@v1
  with:
    url: ${{ secrets.SHIPIT_URL }}
    secret: ${{ secrets.SHIPIT_SECRET }}
    project: beyen-home
```

**私有仓库**还需创建一个细粒度 personal access token
（Repository access：目标仓库；Permissions：*Contents: Read-only*），
填入配置的 `token:` 字段（全局或按项目）。
用 `shipit check` 检查其过期时间。

## 日常使用

```bash
git tag v1.2.0 && git push --tags      # 构建、发布、部署
```

在服务器上（以 root 或加 `sudo` 前缀）：

```bash
shipit status                  # 每个项目的版本、服务状态和运行账号（`!` 标出 shipit 和 root）
shipit list beyen-home         # 磁盘上已有的 Release 列表
shipit deploy beyen-home       # 部署最新 GitHub Release
shipit deploy beyen-home v1.2.0  # 部署指定版本（磁盘上已有则不下载）
shipit rollback beyen-home     # 回到上一版本
shipit rollback beyen-home v1.0.0  # 回到磁盘上某个版本，永不下载
shipit check [-offline]        # 验证全部配置
```

`-c <file>`（命令前）或 `$SHIPIT_CONFIG` 可指定其他配置文件。

注意事项：

- 连续两次 `rollback` 会在同两个版本之间来回切换。
- 回滚到旧版本也是正常的切换流程：同样会重启并进行健康检查。
- 磁盘上只保留 `keep` 个 Release（默认 5）；当前和上一版本永不删除。
- 若同一 tag 重新发布，需先在服务器上删除 `releases/<tag>` 再重新 deploy。
- 回滚只回退代码，不回退数据。请保持数据库迁移的向后兼容性。

也可以不登录服务器直接回滚：将
[`examples/workflows/rollback.yml`](examples/workflows/rollback.yml) 添加到仓库，
从 Actions 标签页手动运行即可。

## 配置参考

`/etc/shipit/shipit.yaml`，权限 `0600`，属主 `shipit`。未知键会报错。

| 键 | | |
|---|---|---|
| `listen` | 可选 | webhook 监听地址，默认 `:7447` |
| `secret` | serve 时必填 | 与 CI 共享，至少 16 个字符 |
| `token` | 可选 | GitHub token，所有项目的默认值 |
| `projects.<name>.type` | 必填 | `go`、`vite` 或 `next` |
| `projects.<name>.repo` | 必填 | `owner/name` |
| `projects.<name>.dir` | 必填 | 项目目录的绝对路径 |
| `projects.<name>.service` | go/next 必填；vite 禁止填写 | 切换后重启的 systemd unit |
| `projects.<name>.health` | 可选 | 重启后必须返回 2xx 的 http(s) URL |
| `projects.<name>.health_timeout` | 可选 | 默认 `30s`，最长 `10m` |
| `projects.<name>.asset` | 可选 | 包名前缀，默认为项目名 |
| `projects.<name>.keep` | 可选 | 磁盘保留的 Release 数，默认 `5`，最小 `2` |
| `projects.<name>.token` | 可选 | 覆盖全局 token |

项目目录不能重叠，服务名不能共用。

## Webhook API

除 `/healthz` 外，所有端点均需签名。请求体为 JSON。

| | |
|---|---|
| `POST /v1/deploy` | `{"project": "...", "tag": "v1.2.0"}`（tag 必填） |
| `POST /v1/rollback` | `{"project": "...", "tag": "v1.1.0"}`（tag 可选，默认为上一版本） |
| `GET /v1/jobs/<id>` | 任务状态和日志 |
| `GET /healthz` | 存活检查，无需认证 |

默认 `POST` 返回 `202` 和 `job_id`；轮询该 job 直到 `status` 为 `success` 或 `failed`。
加 `?wait=true` 则请求保持连接并返回最终结果（成功 `200`，失败 `500`，项目繁忙 `409`）。
`action` 和 `scripts/call.sh` 已为你处理轮询逻辑。

签名：发送 `X-Shipit-Timestamp`（Unix 秒）和 `X-Shipit-Signature`：

```
hex( HMAC-SHA256( secret, timestamp + "\n" + METHOD + "\n" + path + "\n" + body ) )
```

```bash
ts=$(date +%s); body='{"project":"beyen-home","tag":"v1.2.0"}'
sig=$(printf '%s\n%s\n%s\n%s' "$ts" POST /v1/deploy "$body" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')
curl -X POST "$URL/v1/deploy" -H "X-Shipit-Timestamp: $ts" -H "X-Shipit-Signature: $sig" -d "$body"
```

Job 在内存中保留一小时。若 shipit 重启，job 记录消失；
项目的实际状态始终可通过 `shipit status` 查看。

## 安全模型

- **Webhook 不能执行命令。** 它只接受项目名和 tag，校验后仅部署该项目 GitHub 仓库中存在的 Release。命令、URL 和路径均来自配置文件，从不来自请求。
- **请求签名**：HMAC 覆盖 method、path、timestamp 和 body。时间戳必须在 5 分钟内，每个签名只接受一次，捕获的请求无法重放或重定向到其他端点。secret 不经网络传输，这点在 webhook 默认使用 HTTP 时尤为重要。如需隐藏请求内容，可在前面加 TLS 代理，或为 GitHub runner 配置防火墙规则。
- **频率限制**（按客户端 IP）：10 请求/秒（突发 20），1 分钟内 10 次认证失败的 IP 将被锁定剩余时间。全局限制会让任何人都能锁定 CI，所以按 IP 限制。
- **Release 完整性**：每个包都会对照 `checksums.txt` 校验；解包时严格检查路径和符号链接规则、大小限制，不允许覆盖。这能防止数据损坏，但无法防范有权限向你仓库发布 Release 的攻击者——那个访问权限本身才是真正的安全边界。
- **每个服务一个账号。** shipit 和各个服务以不同账号运行。unit 必须有非 root 的 `User=`；`shipit check` 对会以 root 运行的 unit 报错。每次 restart 之前，shipit 还会通过 `systemctl show` 再验证一次（账号必须存在且不能是 uid 0），所以即使不运行 `check` 也能保证这一点。唯一的 root 能力是你添加的精确 `sudo systemctl restart <service>` 行。
- **密钥不在 shipit 够得着的地方。** `.env` 和配置文件放在 `/etc/<service>`，属主为 root；systemd 以 root 身份读取 `EnvironmentFile=`。`shipit check` 会对 shipit 用户能读到的每个 `EnvironmentFile=` 给出警告，`shipit status` 会标出仍以 `shipit` 运行的服务。
- **配置文件**：配置（secret 和 token）必须为 `0600`；否则 shipit 拒绝启动服务。
- **项目目录** 为 `shipit:<服务所在组>`，权限 `2750`：shipit 可写，服务只读，其他人无法进入。shipit 解包时保证该组始终能读取和运行发布内容，不受 umask 或压缩包内权限的影响。shipit 不会强制检查它未创建的目录权限，请在初始化项目时手动设置。
- Webhook 优雅关闭：正在进行的 deploy 完成后进程才退出。

被攻破的服务不再能波及其他服务：既读不到它们的密钥，也改不了它们的代码。
被攻破的 shipit 仍然可以向它管理的每个服务部署代码（这本来就是它的职责），
但读不到它们的密钥和数据。仍以 `shipit` 运行的服务与 shipit 以及彼此共用一个信任边界，
所以 `status` 会标出它们。

## 卸载

```bash
sudo shipit uninstall                  # 删除二进制和 shipit.service；保留配置和项目
sudo shipit uninstall -purge           # 同上，另外删除 /etc/shipit，可选删除 shipit 用户
sudo shipit uninstall -purge -delete-projects   # 同上，另外删除项目目录（需二次确认）
```

加 `-dry-run` 预览操作，加 `-y` 跳过确认。
shipit 不会删除它未创建的文件，如 `/etc/sudoers.d/shipit` 和你的项目 systemd unit；
它会列出这些文件供你手动清理。
只有看起来像 shipit 项目（真实目录且含 `releases/` 子目录）的目录才会被删除。

## 故障排查

| 现象 | 原因 |
|---|---|
| CI: `rejected: bad signature or clock skew` | `SHIPIT_SECRET` 错误，或服务器时钟偏差超过 5 分钟（启用 NTP） |
| CI: `429` | 触发频率限制，或该 IP 失败次数过多；等待一分钟 |
| `404 ... private repos need a token` | tag/仓库错误，或 token 无法读取该仓库 |
| `release has no asset ...` | 包名与上表不符；错误信息会列出 Release 中实际包含的文件 |
| `built for EM_AARCH64 but this server is amd64` | 打包了错误的架构 |
| `missing .next/static` | Next.js 包构建时未复制 `.next/static` |
| `sudo: a password is required` | sudoers 行缺失或与命令不完全匹配 |
| `refusing to restart ...: unit runs as root` | unit 没有 `User=`（或是 `User=root`）；给它一个独立的非 root 账号，然后 `systemctl daemon-reload` |
| 服务启动后报文件 `Permission denied` | 服务账号读不了发布内容：项目目录的属组必须是 `svc-<name>`、权限 `2750`，目录里的文件也要是同一个组（`sudo chgrp -R`） |
| `... rolled back to vX` | 新版本重启或健康检查失败；查看 `journalctl -u <service>` |
| `another operation is in progress` | 该项目有 deploy 正在进行 |

日志：`journalctl -u shipit -f`。

## 发布 shipit 自身

推送 `vX.Y.Z` tag。workflow 会构建 `shipit-linux-amd64`、`shipit-linux-arm64` 和
`checksums.txt`，发布 Release，并更新 `uses: beyenpay/shipit@v1` 使用的浮动主版本 tag（`v1`）。

## 许可证

MIT

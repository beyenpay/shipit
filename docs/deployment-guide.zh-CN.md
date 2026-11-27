# 部署指南

在新服务器上部署 shipit 并添加第一个项目的完整步骤。

---

## 第一部分 — 在新服务器上安装 shipit

### 1.1 前提条件

- Linux 服务器（推荐 Debian/Ubuntu），amd64 或 arm64 架构
- root 权限（或有 `sudo` 的用户）
- 可对外访问 GitHub 的 HTTPS（443 端口）
- 一个用于 webhook 的入站 TCP 端口（默认 `7447`）

### 1.2 运行安装脚本

```bash
curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

安装脚本会：
- 创建 `shipit` 系统用户（不可登录 shell）
- 安装 `/usr/local/bin/shipit`（属主 root）
- 写入 `/etc/shipit/shipit.yaml`，自动生成随机 secret
- 安装并启动 `shipit.service`（以 `User=shipit` 运行）

如需固定版本：

```bash
SHIPIT_VERSION=v1.2.3 curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
```

### 1.3 验证安装

```bash
systemctl status shipit          # 应为 active (running)
sudo shipit check                # 验证完整安装
```

未添加任何项目时，`shipit check` 会通过所有安装检查。

### 1.4 编辑配置

```bash
sudo -u shipit vi /etc/shipit/shipit.yaml
```

最简配置（项目在第二部分添加）：

```yaml
listen: :7447
secret: <随机生成 — 除非需要轮换，否则不要修改>
```

如果有私有仓库，添加全局 GitHub token：

```yaml
token: github_pat_...
```

### 1.5 开放防火墙

允许来自 GitHub Actions runner IP 或任意来源的 webhook 端口入站 TCP：

```bash
# ufw 示例
sudo ufw allow 7447/tcp
```

GitHub runner 的 IP 段可在
`https://api.github.com/meta`（键名：`actions`）查询。

---

## 第二部分 — 添加新项目

以下以 Next.js 应用 `beyen-home` 为例。
根据实际情况调整名称、路径和端口。

### 2.1 在配置文件中添加项目

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

配置在下次请求时自动生效，无需重启 shipit。

### 2.2 创建服务账号和目录

shipit 负责部署，服务以自己独立的账号运行，这样被攻破的服务就够不到其他服务。
每类文件只有一个位置、一个属主：

| 路径 | 内容 | 属主 : 权限 |
|---|---|---|
| `/srv/app/beyen-home/web` | `releases/`、`current`、`previous`：只放 shipit 会碰的东西 | `shipit:svc-beyen-home` `2750` |
| `/etc/beyen-home` | `.env` 和配置文件 | `root:svc-beyen-home` `0750` |
| `/etc/beyen-home/.env` | 密钥 | `root:root` `0600` |
| `/var/lib/beyen-home` | 程序保存的数据 | 由 systemd 创建（`StateDirectory=`），属于服务账号 |
| `/var/log/beyen-home` | 日志文件 | 由 systemd 创建（`LogsDirectory=`），属于服务账号 |

```bash
# 服务账号：不能登录，没有家目录
sudo useradd --system --no-create-home --shell /usr/sbin/nologin svc-beyen-home
id svc-beyen-home                      # 同名的组必须存在

# 项目目录：shipit 可写，服务只读，其他人进不来
sudo mkdir -p /srv/app/beyen-home/web
sudo chown shipit:svc-beyen-home /srv/app/beyen-home/web
sudo chmod 2750 /srv/app/beyen-home/web      # setgid：每个新版本都继承该组
ls -ld /srv/app /srv/app/beyen-home          # 必须属于 root，这样 shipit 无法替换项目目录

# 配置：属于 root，服务可读但不能改
sudo install -d -o root -g svc-beyen-home -m 750 /etc/beyen-home
sudo touch /etc/beyen-home/.env && sudo chmod 600 /etc/beyen-home/.env
sudo vi /etc/beyen-home/.env
```

为什么这样设计：

- shipit 能写代码而服务不能，所以被攻破的服务无法植入重启后仍然存在的代码。
- shipit 永远看不到密钥。systemd 在启动服务之前以 root 身份读取 `EnvironmentFile=`，
  所以 `.env` 可以是 `root:root 0600`。如果程序是自己打开文件而不是读环境变量，
  把它设为 `root:svc-beyen-home` `0640`。
- 被攻破的服务仍然拿得到它自己的密钥（进程要用），但拿不到其他服务的。
  请给每个服务单独的数据库账号和密钥，怀疑被攻破时立即轮换。
- 程序只能写入 `/var/lib/beyen-home` 和 `/var/log/beyen-home`，或者把日志输出到 stdout
  （由 journald 收集）。写入自己目录里的任何东西（如 `./data`、`./log`）都会失败，
  因为该目录对它是只读的。

`vite` 项目没有服务，因此也没有账号：把目录的属组设为 nginx 所在的组即可，
`sudo chown shipit:www-data <dir> && sudo chmod 2750 <dir>`（Debian/Ubuntu 上是 `www-data`）。

### 2.3 创建 systemd unit

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

# systemd 创建这些目录并交给服务账号
StateDirectory=beyen-home
StateDirectoryMode=0700
LogsDirectory=beyen-home

# 沙箱：除上面两个目录和 /tmp 外，其余全部只读
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes

[Install]
WantedBy=multi-user.target
```

`User=` 必须设置且不能是 root：shipit 会拒绝重启这样的 unit，`shipit check` 也会报错。
不要让多个服务共用一个账号。上面的目录不要再写 `ReadWritePaths=`：`StateDirectory=` 和
`LogsDirectory=` 已经让它们可写，而对不存在的路径写 `ReadWritePaths=` 会导致服务无法启动。

启用（**不要** start——首次部署前 `current/` 还不存在）：

```bash
sudo systemctl daemon-reload && sudo systemctl enable beyen-home
```

### 2.4 添加 sudoers 规则

```bash
sudo visudo -f /etc/sudoers.d/shipit
```

每个服务精确添加一行——不能用通配符：

```
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home
```

### 2.5 运行验证

```bash
sudo shipit check
```

继续之前，所有检查项必须全部通过。修复所有 `✘` 项。`!` 是警告，不会阻止部署，但值得处理，尤其是：

- `env file ... can be read by shipit`：密钥在 shipit 够得着的地方。
  把文件移到 `/etc/<service>/`，属主改为 root。
- `... runs as shipit, like shipit and every other project that does`：服务仍与 shipit 共用账号
  （见[迁移已有项目](#迁移已有项目)）。

`sudo shipit status` 的 `RUNNER` 列显示每个服务以哪个账号运行，`shipit` 和 `root` 会被标上 `!`。

### 2.6 在 GitHub 仓库中添加 Secrets

在项目仓库 → Settings → Secrets and variables → Actions：

| Secret 名称 | 值 |
|-------------|-----|
| `SHIPIT_URL` | `http://<服务器IP>:7447` |
| `SHIPIT_SECRET` | `/etc/shipit/shipit.yaml` 中的 `secret` 值 |

私有仓库还需添加：

| Secret 名称 | 值 |
|-------------|-----|
| `SHIPIT_TOKEN` | 细粒度 PAT，*Contents: Read-only*，仅限本仓库 |

并在配置中设置：

```yaml
projects:
  beyen-home:
    token: github_pat_...   # 直接填入值
```

### 2.7 添加 GitHub Actions workflow

从 [`examples/workflows/`](../examples/workflows/) 复制对应的示例文件
到项目仓库的 `.github/workflows/release.yml`，修改顶部的变量。

workflow 中的 deploy 步骤：

```yaml
- uses: beyenpay/shipit@v1
  with:
    url: ${{ secrets.SHIPIT_URL }}
    secret: ${{ secrets.SHIPIT_SECRET }}
    project: beyen-home
```

### 2.8 执行第一次部署

在本地机器的项目仓库中：

```bash
git tag v1.0.0
git push origin v1.0.0
```

接下来发生的事情：

1. GitHub Actions 因新 tag 触发
2. workflow 构建 release 包并发布到 GitHub Releases
3. workflow 向服务器发送 `POST /v1/deploy`（HMAC 签名）
4. shipit 下载 release，校验 checksum，解包
5. shipit 检查二进制/包的类型和 CPU 架构
6. `current` 符号链接切换到新 release
7. 执行 `sudo systemctl restart beyen-home`
8. 轮询健康 URL 直到返回 2xx（或超时）
9. 若健康检查失败，切回 `previous` 并重启
10. CI 任务显示真实的成功或失败结果

实时查看部署过程：

```bash
journalctl -u shipit -f          # shipit 自身的日志
journalctl -u beyen-home -f      # 已部署应用的日志
```

### 2.9 后续部署

每个新 tag 都会触发完整的部署流程：

```bash
git tag v1.0.1
git push origin v1.0.1
```

从服务器手动部署（适合调试）：

```bash
sudo shipit deploy beyen-home            # GitHub 上的最新 release
sudo shipit deploy beyen-home v1.0.0     # 指定版本
sudo shipit rollback beyen-home          # 回到上一版本
```

## 迁移已有项目

按旧方式配置的项目（以 `shipit` 运行，`shared/.env` 在项目目录里）可以继续工作。
请一次迁移一个，先从最不重要的开始。

先升级 shipit，因为旧版本会拒绝不以 `shipit` 运行的 unit：`sudo shipit self-update`。
然后，以 `/srv/app/beyen-api` 里的服务 `beyen-api` 为例：

```bash
SVC=beyen-api; DIR=/srv/app/beyen-api; ACCT=svc-$SVC

sudo cp -a "$DIR" /root/$SVC.bak                     # 留一条退路
sudo useradd --system --no-create-home --shell /usr/sbin/nologin $ACCT
id $ACCT
sudo systemctl stop $SVC                             # 停机从这里开始

# 配置 -> /etc，属于 root
sudo install -d -o root -g $ACCT -m 750 /etc/$SVC
sudo mv "$DIR/shared/.env" /etc/$SVC/.env
sudo chown root:root /etc/$SVC/.env && sudo chmod 600 /etc/$SVC/.env
# 其他配置文件：sudo mv "$DIR/shared/config.yaml" /etc/$SVC/，再
#   sudo chown root:$ACCT /etc/$SVC/config.yaml && sudo chmod 640 /etc/$SVC/config.yaml

# 数据和日志 -> /var，属于服务账号
sudo install -d -o $ACCT -g $ACCT -m 700 /var/lib/$SVC
sudo install -d -o $ACCT -g $ACCT -m 750 /var/log/$SVC
sudo sh -c "mv '$DIR'/shared/data/* /var/lib/$SVC/"         # 按你的程序实际保存的内容调整
sudo sh -c "mv '$DIR'/shared/logs/* /var/log/$SVC/"
sudo chown -R $ACCT:$ACCT /var/lib/$SVC /var/log/$SVC

# 项目目录：shipit 可写，服务只读
sudo chgrp -R $ACCT "$DIR"
sudo chmod -R g+rX "$DIR"
sudo find "$DIR" -type d -exec chmod g+s {} +        # releases/ 也必须继承该组
sudo chmod 2750 "$DIR"
sudo chown root:root /srv/app                        # $DIR 上面的每一级目录同理
```

然后按 2.3 节修改 unit（`sudo systemctl edit --full $SVC`）：

| 旧 | 新 |
|---|---|
| `User=shipit`、`Group=shipit` | `User=svc-beyen-api`、`Group=svc-beyen-api` |
| `EnvironmentFile=<dir>/shared/.env` | `EnvironmentFile=/etc/beyen-api/.env` |
| `ReadWritePaths=<dir>/shared/...` | 删除；加上 `StateDirectory=beyen-api` 和 `LogsDirectory=beyen-api` |

让应用指向新位置：`.env` 里（或写死在代码里）的 `LOG_DIR`、`DATA_DIR` 和配置文件路径，
现在应是 `/var/log/beyen-api`、`/var/lib/beyen-api` 和 `/etc/beyen-api/...`。
指向 `current/` 内部的相对路径会失败，因为该目录对服务是只读的。

```bash
sudo systemctl daemon-reload
sudo shipit check                                    # 没有 ✘；没有 env 和共用账号的警告
sudo systemctl start $SVC
```

删除任何东西之前先验证：

```bash
sudo shipit status                                   # RUNNER 列显示 svc-beyen-api，没有 "!"
sudo journalctl -u $SVC -n 50
sudo -u shipit cat /etc/$SVC/.env                    # 必须失败，提示 "Permission denied"
```

部署一个版本（或 `sudo shipit deploy $SVC`）一次，确认服务能读取新版本。如果出了问题，
停掉服务，把 `/root/$SVC.bak` 放回去并恢复旧 unit。稳定运行几天之后，删除备份
（`/root/$SVC.bak` 里还有旧的 `.env`）和空的 `$DIR/shared`。

---

## 核对清单

完成以上两个部分后运行。标有 [自动] 的项目由 `shipit check` 自动验证。

### shipit 安装

- [ ] `systemctl status shipit` — active (running)
- [ ] `sudo shipit check` — 全部绿色 [自动]
- [ ] `getent passwd shipit | cut -d: -f7` — shell 为 `/usr/sbin/nologin` 或 `/bin/false`
- [ ] `sudo stat /etc/shipit/` — 权限 `0700`，属主 `shipit:shipit`
- [ ] `sudo stat /etc/shipit/shipit.yaml` — 权限 `0600`，属主 `shipit:shipit`
- [ ] `stat /etc/sudoers.d/shipit` — 权限 `0440`，属主 `root:root`
- [ ] sudoers 中每行都是**精确的**服务名，无通配符

### 每个项目

- [ ] `stat /srv/app/<项目>` — 权限 `2750`，属主 `shipit:svc-<service>`；其上层目录属于 `root`
- [ ] `sudo stat /etc/<service>/.env` — 权限 `0600`，属主 `root:root`（程序自己读文件时用 `root:svc-<service>` `0640`）
- [ ] `sudo -u shipit cat /etc/<service>/.env` — 失败，提示 *Permission denied*
- [ ] `systemctl show <service> -p User` — 该服务自己的非 root 账号，不是 `shipit`
- [ ] `sudo shipit check` — unit、sudoers、仓库通过，且没有“env 文件可读”的警告 [自动]
- [ ] `sudo shipit status` — `RUNNER` 列显示服务账号，没有 `!`
- [ ] GitHub 仓库中已设置 `SHIPIT_URL` 和 `SHIPIT_SECRET`
- [ ] 首次部署成功：`sudo shipit status`

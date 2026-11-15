# 部署指南

在新服务器上部署 shipit 并添加第一个项目的完整步骤。

---

## 第一部分 — 在新服务器上安装 shipit

### 1.1 前提条件

- Linux 服务器（推荐 Debian/Ubuntu），amd64 或 arm64 架构
- root 权限（或有 `sudo` 的用户）
- 可对外访问 GitHub 的 HTTPS（443 端口）
- 一个用于 webhook 的入站 TCP 端口（默认 `9000`）

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
listen: :9000
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
sudo ufw allow 9000/tcp
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

### 2.2 创建项目目录

```bash
sudo mkdir -p /srv/app/beyen-home/web/shared
sudo chown -R shipit:shipit /srv/app/beyen-home
sudo chmod 750 /srv/app/beyen-home
```

在首次部署前创建 `.env` 文件，确保属主和权限正确：

```bash
sudo -u shipit touch /srv/app/beyen-home/web/shared/.env
sudo chmod 600 /srv/app/beyen-home/web/shared/.env
sudo -u shipit vi /srv/app/beyen-home/web/shared/.env
```

### 2.3 创建 systemd unit

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

继续之前，所有检查项必须全部通过。修复所有 `✘` 项。

### 2.6 在 GitHub 仓库中添加 Secrets

在项目仓库 → Settings → Secrets and variables → Actions：

| Secret 名称 | 值 |
|-------------|-----|
| `SHIPIT_URL` | `http://<服务器IP>:9000` |
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

- [ ] `stat /srv/app/<项目>` — 权限 `0750`，属主 `shipit:shipit`
- [ ] `sudo stat /srv/app/<项目>/shared/.env` — 权限 `0600`，属主 `shipit:shipit`
- [ ] `systemctl show <service> -p User` — 返回 `User=shipit`
- [ ] `sudo shipit check` — unit、sudoers、仓库全部通过 [自动]
- [ ] GitHub 仓库中已设置 `SHIPIT_URL` 和 `SHIPIT_SECRET`
- [ ] 首次部署成功：`sudo shipit status`

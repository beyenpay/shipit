# shipit 安全检查清单

**适用时机：** 首次安装 shipit，或每次新增一个项目时使用。
按顺序逐项检查。标有 **[自动]** 的项目已由 `shipit check` 覆盖；其余需人工核查。

---

## 第一部分 — `shipit check` 已覆盖的检查

先跑一次，把所有 `✘` 修完再继续。

```bash
sudo shipit check
```

| 检查项 | 验证内容 |
|--------|---------|
| 配置文件权限 | `/etc/shipit/shipit.yaml` 是否为 `0600`（本地其他用户不可读） |
| Secret 强度 | `secret` 是否至少 16 个字符 |
| Webhook 端口 | 端口是空闲、shipit 在监听，还是被其他进程占用 |
| GitHub Token | Token 是否有效；14 天内过期则警告 |
| 项目目录 | 目录是否存在且当前用户可写 |
| GitHub 仓库 | 仓库是否可用配置的 Token 访问 |
| Systemd 单元 | 单元是否已加载；`User=shipit`；`WorkingDirectory=<dir>/current` |
| Sudoers 规则 | `sudo -n systemctl restart <service>` 是否免密允许 |

**`shipit check` 不覆盖的内容：**

- 项目目录权限（`0750` vs 世界可读的 `0755`）
- `shared/.env` 是否存在及权限（`0600`）
- `/etc/sudoers.d/shipit` 文件自身权限
- Node.js 运行时是否已安装（`type: next` 必需）
- `www-data` 是否在 `shipit` 组（`type: vite` 配 nginx 时必需）
- TLS / Webhook 端口的网络暴露情况

以下手动步骤补全这些空白。

---

## 第二部分 — 安装检查清单（每台服务器一次）

### 2.1 shipit 二进制与服务

```bash
stat /usr/local/bin/shipit
# 期望：Uid: root  Mode: 0755
```

- [ ] `/usr/local/bin/shipit` — 属主 `root:root`，权限 `0755`
- [ ] `/etc/systemd/system/shipit.service` — 属主 `root:root`，权限 `0644`
- [ ] `shipit.service` 包含 `User=shipit`（绝对不能是 `User=root`）
- [ ] `shipit.service` **不含** `NoNewPrivileges=yes`（shipit 需要 sudo 来重启项目服务）

### 2.2 shipit 用户与配置目录

```bash
id shipit                              # 用户存在，shell 为 /usr/sbin/nologin
stat /etc/shipit
stat /etc/shipit/shipit.yaml
stat /var/lib/shipit
```

- [ ] `shipit` 用户存在，使用不可登录的 shell（`/usr/sbin/nologin` 或 `/bin/false`）
- [ ] `/etc/shipit/` — 属主 `shipit:shipit`，权限 `0700`
- [ ] `/etc/shipit/shipit.yaml` — 属主 `shipit:shipit`，权限 `0600`
- [ ] `/var/lib/shipit/` — 属主 `shipit:shipit`，权限 `0700`

### 2.3 Sudoers 文件

```bash
stat /etc/sudoers.d/shipit
cat  /etc/sudoers.d/shipit
```

- [ ] `/etc/sudoers.d/shipit` — 属主 `root:root`，权限 `0440`
- [ ] 每一行都**精确指定服务名** — 不能用通配符，不能裸写 `/usr/bin/systemctl`

```
# 正确 — 命令精确，服务名精确
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart beyen-home

# 错误 — 通配符；允许 shipit 重启任意服务，包括以 root 运行的服务
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl restart *
shipit ALL=(root) NOPASSWD: /usr/bin/systemctl
```

### 2.4 Webhook Secret

```bash
grep secret /etc/shipit/shipit.yaml
```

- [ ] `secret` 至少 32 个随机十六进制字符（安装程序自动生成）
- [ ] 同一值存储在 GitHub Secret `SHIPIT_SECRET` 中 — 仅此一处
- [ ] Secret 从未出现在 git 提交、日志文件或错误信息中

---

## 第三部分 — 新项目检查清单（每个项目一次）

对 `shipit.yaml` 中每个新增项目重复以下步骤。

### 3.1 项目目录

```bash
PROJECT_DIR=/srv/app/beyen-home/web    # 按实际路径调整

stat "$PROJECT_DIR"
stat "$PROJECT_DIR/shared"
```

- [ ] 项目目录 — 属主 `shipit:shipit`，权限 **`0750`**（不能是 `0755`）
- [ ] `shared/` 子目录 — 属主 `shipit:shipit`，权限 **`0750`**

```bash
# 正确创建方式
sudo mkdir -p "$PROJECT_DIR/shared"
sudo chown -R shipit:shipit "$PROJECT_DIR"
sudo chmod 750 "$PROJECT_DIR"
```

> **为什么是 0750？**  
> `0755` 允许服务器上所有本地用户读取已部署的源码并进入 `shared/`。  
> `0750` 仅允许 `shipit` 组成员访问，本机其他用户无法进入。

### 3.2 `shared/.env` 文件

```bash
stat "$PROJECT_DIR/shared/.env"
```

- [ ] `shared/.env` — 属主 `shipit:shipit`，权限 **`0600`**

```bash
# 正确创建方式
sudo -u shipit touch "$PROJECT_DIR/shared/.env"
sudo chmod 600 "$PROJECT_DIR/shared/.env"
sudo -u shipit vi "$PROJECT_DIR/shared/.env"
```

> **为什么是 0600 而不是 0640？**  
> 即使父目录是 `0750`，`www-data` 等组成员仍然能读取 `0640` 的文件。  
> `0600` 是仅限属主（shipit）和 root 可读，彻底切断了组成员的读取路径。

### 3.3 Systemd 单元

```bash
systemctl show beyen-home -p LoadState -p User -p WorkingDirectory
```

- [ ] `LoadState=loaded`
- [ ] `User=shipit` — **不能为空，不能为 `root`**
- [ ] `WorkingDirectory=<dir>/current`

> `shipit check` 已验证上述三项。  
> 此外，经过最近的安全更新，shipit 在每次部署重启前会通过 `systemctl show -p User`
> 进行运行时验证，单元非 `shipit` 用户时拒绝 restart——这是运行时强制约束，
> 而非仅依赖人工 check。

### 3.4 此服务的 Sudoers 规则

```bash
sudo -u shipit sudo -n -l /usr/bin/systemctl restart beyen-home
```

- [ ] 命令退出码为 `0`（允许免密执行）
- [ ] 规则精确命名 `beyen-home` — 不能匹配其他服务

### 3.5 运行时依赖（按项目类型）

| 类型 | 服务器要求 | 验证命令 |
|------|-----------|---------|
| `go` | 无 — 静态二进制 | `file releases/v*/myapp` → `ELF 64-bit` |
| `vite` | nginx 或类似服务 | `nginx -t` |
| `next` | Node.js 运行时 | `node --version` |

**`type: next` 时：**

- [ ] Node.js 已安装：`which node && node --version`
- [ ] 版本满足 Next.js 要求（查看 `.nvmrc` 或 `package.json engines`）
- [ ] 单元文件 `ExecStart` 使用完整路径：`/usr/bin/node server.js`

**`type: vite` 配 nginx 时：**

- [ ] nginx 的系统用户（Debian/Ubuntu 上为 `www-data`）已加入 `shipit` 组：
  ```bash
  groups www-data   # 应包含 shipit
  # 若未包含：
  sudo usermod -aG shipit www-data && sudo systemctl reload nginx
  ```
- [ ] 此设置允许 nginx 读取 `0750` 项目目录中的静态文件，同时 `.env` 因为是 `0600` 仍然对 nginx 不可见

### 3.6 GitHub 仓库与 Token

```bash
sudo shipit check
```

- [ ] `shipit check` 显示仓库可访问
- [ ] Token 权限范围为 **Contents: read-only**，仅限目标仓库，不超出
- [ ] 私有仓库优先使用 `token:` 配置每项目独立 Token，避免全局 Token 泄露影响所有项目

---

## 第四部分 — 完整权限速查表

| 路径 | 属主 | 权限 | 由谁设置 |
|------|------|------|---------|
| `/usr/local/bin/shipit` | `root:root` | `0755` | install.sh |
| `/usr/local/bin/shipit.old` | `root:root` | `0755` | self-update |
| `/etc/systemd/system/shipit.service` | `root:root` | `0644` | install.sh |
| `/etc/systemd/system/<项目>.service` | `root:root` | `0644` | 管理员 |
| `/etc/shipit/` | `shipit:shipit` | `0700` | install.sh |
| `/etc/shipit/shipit.yaml` | `shipit:shipit` | `0600` | install.sh |
| `/etc/sudoers.d/shipit` | `root:root` | `0440` | 管理员 |
| `/var/lib/shipit/` | `shipit:shipit` | `0700` | install.sh |
| `/srv/app/<项目>/` | `shipit:shipit` | `0750` ⚠ | 管理员 |
| `/srv/app/<项目>/releases/` | `shipit:shipit` | `0750` | shipit 自动 |
| `/srv/app/<项目>/shared/` | `shipit:shipit` | `0750` ⚠ | 管理员 |
| `/srv/app/<项目>/shared/.env` | `shipit:shipit` | `0600` ⚠ | 管理员 |
| `/srv/app/<项目>/.shipit.lock` | `shipit:shipit` | `0600` | shipit 自动 |

⚠ = shipit 不自动强制，必须手动设置。

---

## 第五部分 — RCE 风险矩阵

列出攻击者可能执行任意代码的每条路径、所需前提条件，以及对应防控措施。

### 严重 — 直接通往 root

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R1 | Sudoers 规则过宽（如 `systemctl restart *`）→ 重启一个以 root 运行的服务 → 该服务执行攻击者控制的代码 | 获取 `shipit` 用户权限（通过 Webhook 或本地） | **每条 sudoers 规则精确到服务名，不用通配符** |
| R2 | 单元文件含 `User=root` 或无 `User=` → 部署恶意 Release → restart → 以 root 执行 | HMAC Secret + 单元文件配置错误 | **shipit 现在在每次 restart 前检查单元 `User=`**（部署时强制）；`shipit check` 也做同样校验 |
| R3 | 供应链攻击：`beyenpay/shipit` GitHub 账号被攻破 → Release 中植入恶意二进制 → `self-update` 安装 → 以 root 执行 | 攻击 shipit 仓库或其 CI | 锁定 `SHIPIT_VERSION`；高安全要求环境在升级前人工审查二进制 |

### 高危 — 以 `shipit` 用户执行代码

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R4 | HMAC Secret 泄露 → 攻击者调 `POST /v1/deploy` 传入恶意 tag → 任意代码以 `shipit` 用户运行 | Secret 泄露（CI Secrets、日志、网络嗅探） | 泄露后立即轮换 Secret；TLS 代理隐藏请求内容 |
| R5 | `.env` 权限为 `0644`（世界可读）→ 攻击者读取数据库密码 / API Key → 凭证被用于攻击后端系统 | 同机本地用户，或服务器上任意进程 | **`chmod 600 shared/.env`** |

### 中危 — 信息泄露或横向移动

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R6 | 项目目录权限为 `0755`（世界可读）→ 本地用户读取部署的源码 → 发现硬编码 Secret 或可利用逻辑 | 同机本地用户 | **`chmod 750 /srv/app/<项目>`** |
| R7 | 一个项目的运行代码被攻破 → 读取所有其他项目的 `shared/.env`（所有项目共用 `shipit` 用户） | 任意一个项目代码被攻破 | 架构层面的权衡：单用户 = 单一信任边界。每个 `.env` 保持 `0600` 是最有效的缓解手段 |

### 低危 — 已被遏制或需认证

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R8 | Release 压缩包路径穿越 → 写文件到项目目录之外 | 攻击者控制 GitHub Release | 严格的解压实现（绝对路径、`..`、指向 dest 外的符号链接均拒绝） |
| R9 | 重放攻击：捕获签名请求 → 再次提交 | 5 分钟窗口内的合法签名请求 | 重放缓存；时间戳强制 5 分钟窗口 |
| R10 | 绕过频率限制 → 暴力破解 HMAC Secret | 直连 Webhook 端口 | 按 IP 频率限制；10 次认证失败 → 锁定 1 分钟 |
| R11 | Health URL 作为 SSRF → 探测内网服务 | 读取 `shipit.yaml` 配置 | Health URL 由管理员配置；探测仅为 GET，不使用响应体 |

### 已完全缓解（无残余风险）

| 攻击手段 | 缓解措施 |
|---------|---------|
| 通过服务名注入命令 | 服务名经严格正则校验；`exec` 不经 shell |
| 部署错误 CPU 架构的二进制 | 激活前检查 ELF 头与服务器架构是否匹配 |
| 解压炸弹 | 解包总大小限制 4 GiB；条目数限制 200,000 |
| 跨端点复用签名 | 签名覆盖 method + path + timestamp + body |
| 并发部署同一项目 | 按项目 `flock(2)` 文件锁 |

---

## 第六部分 — 快速核查脚本

安装 shipit 或新增项目后运行。按实际路径调整。

```bash
#!/usr/bin/env bash
# shipit 权限快速审查脚本
# 用法: bash audit.sh /srv/app/beyen-home/web

set -euo pipefail
PROJECT_DIR="${1:-/srv/app/myproject}"
PASS="  ✔"; FAIL="  ✘"

check() {
  local label="$1" expected="$2" actual="$3"
  if [ "$actual" = "$expected" ]; then
    echo "$PASS $label ($actual)"
  else
    echo "$FAIL $label — 期望 $expected，实际 $actual"
  fi
}

echo "=== shipit 安装 ==="
check "/usr/local/bin/shipit 属主:权限" \
  "root:root 0755" \
  "$(stat -c '%U:%G %a' /usr/local/bin/shipit 2>/dev/null || echo 不存在)"

check "/etc/shipit/ 属主:权限" \
  "shipit:shipit 0700" \
  "$(stat -c '%U:%G %a' /etc/shipit 2>/dev/null || echo 不存在)"

check "/etc/shipit/shipit.yaml 属主:权限" \
  "shipit:shipit 0600" \
  "$(stat -c '%U:%G %a' /etc/shipit/shipit.yaml 2>/dev/null || echo 不存在)"

check "/etc/sudoers.d/shipit 属主:权限" \
  "root:root 0440" \
  "$(stat -c '%U:%G %a' /etc/sudoers.d/shipit 2>/dev/null || echo 不存在)"

echo ""
echo "=== 项目：$PROJECT_DIR ==="
check "$PROJECT_DIR 属主:权限" \
  "shipit:shipit 0750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR" 2>/dev/null || echo 不存在)"

check "$PROJECT_DIR/shared 属主:权限" \
  "shipit:shipit 0750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR/shared" 2>/dev/null || echo 不存在)"

if [ -f "$PROJECT_DIR/shared/.env" ]; then
  check "$PROJECT_DIR/shared/.env 属主:权限" \
    "shipit:shipit 0600" \
    "$(stat -c '%U:%G %a' "$PROJECT_DIR/shared/.env")"
else
  echo "  - $PROJECT_DIR/shared/.env 尚不存在（首次部署前请先创建）"
fi

echo ""
echo "=== shipit check（自动检查）==="
sudo shipit check
```

---

## 总结

`shipit check` 负责**应用层**验证（配置、单元文件、sudoers 规则、GitHub 访问权限）。  
**文件系统层**验证（目录权限、`.env` 权限）不会自动强制执行，必须在初始化时手动设置。

最容易被遗漏、危害最大的三项：

1. **Sudoers 规则含通配符** → 通往 root 的路径（R1）
2. **`.env` 权限为 `0644` 而非 `0600`** → 凭证泄露（R5）
3. **项目目录权限为 `0755` 而非 `0750`** → 本机任意用户可读源码（R6）

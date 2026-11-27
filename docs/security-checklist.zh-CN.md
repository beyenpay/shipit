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
| Systemd 单元 | 单元是否已加载；`User=` 已设置、存在且不是 root（若是 `shipit` 本身则警告）；`WorkingDirectory=<dir>/current` |
| 环境变量文件 | 对单元中 shipit 用户能读到的每个 `EnvironmentFile=` 给出警告 |
| Sudoers 规则 | `sudo -n systemctl restart <service>` 是否免密允许 |

**`shipit check` 不覆盖的内容：**

- 项目目录的属主和权限（`shipit:svc-<service>`、`2750`），以及其上层目录是否属于 root
- `/etc/<service>/` 中除 `EnvironmentFile=` 之外的文件的属主和权限
- `/etc/sudoers.d/shipit` 文件自身权限
- Node.js 运行时是否已安装（`type: next` 必需）
- `type: vite` 配 nginx 时项目目录的属组
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
ls -ld /srv/app /srv/app/beyen-home    # 它的上层目录
```

- [ ] 项目目录 — 属主 `shipit:svc-<service>`，权限 **`2750`**（开头的 `2` 是 setgid）
- [ ] 其上的每一级目录（`/srv/app` 等）— 属主 `root:root`
- [ ] 里面只有 shipit 管理的内容：`releases/`、`current`、`previous`、`.shipit.lock`

```bash
# 正确创建方式
sudo useradd --system --no-create-home --shell /usr/sbin/nologin svc-beyen-home
sudo mkdir -p "$PROJECT_DIR"
sudo chown shipit:svc-beyen-home "$PROJECT_DIR"
sudo chmod 2750 "$PROJECT_DIR"
```

> **为什么是 `shipit:svc-<service>` 和 `2750`？**  
> shipit 写入发布内容；服务账号通过组读取并运行它们，但不能修改，所以被攻破的服务
> 无法留下重启后仍然存在的代码。其他人无法进入。setgid 让每个新版本都继承该组。
> 上层目录归 root，shipit 就无法重命名或替换项目目录。

### 3.2 配置、数据和日志

```bash
sudo stat /etc/beyen-home /etc/beyen-home/.env
sudo -u shipit cat /etc/beyen-home/.env      # 必须失败：Permission denied
```

- [ ] `/etc/<service>/` — 属主 `root:svc-<service>`，权限 **`0750`**
- [ ] `/etc/<service>/.env` — 属主 `root:root`，权限 **`0600`**（程序自己打开文件时用 `root:svc-<service>` `0640`）
- [ ] 其他配置文件（`config.yaml` 等）— 属主 `root:svc-<service>`，权限 **`0640`**：服务可读但不能修改
- [ ] `sudo -u shipit cat /etc/<service>/.env` 失败
- [ ] 数据在 `/var/lib/<service>`（属主为服务账号，权限 `0700`），日志在 `/var/log/<service>`，由 systemd 通过 `StateDirectory=` 和 `LogsDirectory=` 创建；不要再为它们写 `ReadWritePaths=`

```bash
# 正确创建方式
sudo install -d -o root -g svc-beyen-home -m 750 /etc/beyen-home
sudo touch /etc/beyen-home/.env && sudo chmod 600 /etc/beyen-home/.env
sudo vi /etc/beyen-home/.env
```

> **为什么放 `/etc` 和 `/var`，而不是项目目录？**  
> 放在 shipit 地盘里的任何东西，都在攻破 shipit 的人够得着的范围内。systemd 在启动服务前
> 以 root 身份读取 `EnvironmentFile=`，所以密钥根本不需要让 shipit 可读。
> 这**不能**对服务本身隐藏密钥：环境里有密钥的进程被攻破时就会泄露它们。
> 要限制这种损失，请为每个服务使用独立的数据库账号和密钥，怀疑被攻破时立即轮换。

### 3.3 Systemd 单元

```bash
systemctl show beyen-home -p LoadState -p User -p WorkingDirectory -p EnvironmentFiles
```

- [ ] `LoadState=loaded`
- [ ] `User=` 是该服务自己的账号 — **不能为空，不能为 `root`，也不能是 `shipit`**
- [ ] `WorkingDirectory=<dir>/current`
- [ ] `EnvironmentFiles=` 指向 `/etc/<service>/`
- [ ] `NoNewPrivileges=yes`、`ProtectSystem=strict`、`ProtectHome=yes`、`PrivateTmp=yes`、`PrivateDevices=yes`

> `shipit check` 验证前四项。  
> shipit 在每次 restart 前也会通过 `systemctl show -p User` 检查单元，
> `User=` 为空、为 root（或 uid 0）、或在本机不存在时拒绝 restart。
> 这是运行时强制约束，而非仅是事前警告。仍以 `shipit` 运行的单元会被接受，
> 以便逐个迁移项目，但 `shipit check` 会警告，`shipit status` 会用 `!` 标出。

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

- [ ] 项目目录的属组是 nginx 所在的组（Debian/Ubuntu 上为 `www-data`），而不是某个服务组：
  ```bash
  sudo chown shipit:www-data /srv/app/beyen-web
  sudo chmod 2750 /srv/app/beyen-web
  ```
- [ ] 该目录里没有 `.env` 或其他密钥：静态站点本来就没有

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
| `/srv/app/` 及项目上层的每一级目录 | `root:root` | `0755` | 管理员 |
| `/srv/app/<项目>/` | `shipit:svc-<service>` | `2750` ⚠ | 管理员 |
| `/srv/app/<项目>/releases/` 及其下内容 | `shipit:svc-<service>` | `0755`（该组始终可读可运行） | shipit 自动 |
| `/srv/app/<项目>/.shipit.lock` | `shipit:svc-<service>` | `0600` | shipit 自动 |
| `/etc/<service>/` | `root:svc-<service>` | `0750` ⚠ | 管理员 |
| `/etc/<service>/.env` | `root:root` | `0600` ⚠ | 管理员 |
| `/etc/<service>/config.yaml` 等 | `root:svc-<service>` | `0640` ⚠ | 管理员 |
| `/var/lib/<service>/` | `svc-<service>:svc-<service>` | `0700` | systemd（`StateDirectory=`） |
| `/var/log/<service>/` | `svc-<service>:svc-<service>` | `0755`（用 `LogsDirectoryMode=` 可设为 `0750`） | systemd（`LogsDirectory=`） |

⚠ = shipit 不自动强制，必须手动设置。

---

## 第五部分 — RCE 风险矩阵

列出攻击者可能执行任意代码的每条路径、所需前提条件，以及对应防控措施。

### 严重 — 直接通往 root

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R1 | Sudoers 规则过宽（如 `systemctl restart *`）→ 重启一个以 root 运行的服务 → 该服务执行攻击者控制的代码 | 获取 `shipit` 用户权限（通过 Webhook 或本地） | **每条 sudoers 规则精确到服务名，不用通配符** |
| R2 | 单元文件含 `User=root` 或无 `User=` → 部署恶意 Release → restart → 以 root 执行 | HMAC Secret + 单元文件配置错误 | **shipit 在每次 restart 前检查单元 `User=`**，拒绝为空、root、uid 0 或不存在的账号（部署时强制）；`shipit check` 也做同样校验 |
| R3 | 供应链攻击：`beyenpay/shipit` GitHub 账号被攻破 → Release 中植入恶意二进制 → `self-update` 安装 → 以 root 执行 | 攻击 shipit 仓库或其 CI | 锁定 `SHIPIT_VERSION`；高安全要求环境在升级前人工审查二进制 |

### 高危 — 以 `shipit` 或服务账号执行代码

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R4 | HMAC Secret 泄露 → 攻击者调 `POST /v1/deploy` 传入恶意 tag → 任意代码以服务账号运行 | Secret 泄露（CI Secrets、日志、网络嗅探） | 泄露后立即轮换 Secret；TLS 代理隐藏请求内容 |
| R5 | `.env` 权限为 `0644`（世界可读）→ 攻击者读取数据库密码 / API Key → 凭证被用于攻击后端系统 | 同机本地用户，或服务器上任意进程 | **`.env` 放在 `/etc/<service>`，属主 `root:root`，权限 `0600`** |

### 中危 — 信息泄露或横向移动

| # | 攻击路径 | 所需前提 | 防控措施 |
|---|---------|---------|---------|
| R6 | 项目目录权限为 `0755`（世界可读）→ 本地用户读取部署的源码 → 发现硬编码 Secret 或可利用逻辑 | 同机本地用户 | **项目目录 `2750`，属主 `shipit:svc-<service>`** |
| R7 | 一个服务的代码被攻破 → 读取其他服务的 `.env` 和代码，或向它们植入代码 | 任意一个项目代码被攻破 | **每个服务以独立账号运行**：它读不到其他服务的 `/etc`、`/var` 和项目目录，也不能修改自己的代码。残余风险：仍以 `shipit` 运行的服务与 shipit 以及彼此共用一个信任边界；`shipit status` 用 `!` 标出它们，`shipit check` 会警告 |
| R12 | `shipit`（或 webhook）被攻破 → 攻击者向 Release 植入代码并重启服务 → 代码以该服务的账号运行 | R4，或 shipit 自身的漏洞 | 读不到任何服务的密钥和数据（它们在 shipit 够不着的地方）；它唯一的 root 能力是 sudoers 里精确的 restart 行。残余风险：它仍能向自己管理的每个服务部署代码，这本来就是它的职责。请保护好 Secret 和发布流程 |

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
# 用法: bash audit.sh /srv/app/beyen-home/web beyen-home

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
    echo "$FAIL $label — 期望 $expected，实际 $actual"
  fi
}

echo "=== shipit 安装 ==="
check "/usr/local/bin/shipit 属主:权限" \
  "root:root 755" \
  "$(stat -c '%U:%G %a' /usr/local/bin/shipit 2>/dev/null || echo 不存在)"

check "/etc/shipit/ 属主:权限" \
  "shipit:shipit 700" \
  "$(stat -c '%U:%G %a' /etc/shipit 2>/dev/null || echo 不存在)"

check "/etc/shipit/shipit.yaml 属主:权限" \
  "shipit:shipit 600" \
  "$(stat -c '%U:%G %a' /etc/shipit/shipit.yaml 2>/dev/null || echo 不存在)"

check "/etc/sudoers.d/shipit 属主:权限" \
  "root:root 440" \
  "$(stat -c '%U:%G %a' /etc/sudoers.d/shipit 2>/dev/null || echo 不存在)"

echo ""
echo "=== 项目：$PROJECT_DIR（服务 $SERVICE，账号 $ACCT）==="
check "$PROJECT_DIR 属主:权限" \
  "shipit:$ACCT 2750" \
  "$(stat -c '%U:%G %a' "$PROJECT_DIR" 2>/dev/null || echo 不存在)"

check "$(dirname "$PROJECT_DIR") 属主" \
  "root:root" \
  "$(stat -c '%U:%G' "$(dirname "$PROJECT_DIR")" 2>/dev/null || echo 不存在)"

check "/etc/$SERVICE 属主:权限" \
  "root:$ACCT 750" \
  "$(stat -c '%U:%G %a' "/etc/$SERVICE" 2>/dev/null || echo 不存在)"

if [ -f "/etc/$SERVICE/.env" ]; then
  case "$(stat -c '%U:%G %a' "/etc/$SERVICE/.env")" in
    "root:root 600" | "root:$ACCT 640") echo "$PASS /etc/$SERVICE/.env 属主:权限" ;;
    *) echo "$FAIL /etc/$SERVICE/.env — 期望 root:root 600（或 root:$ACCT 640）" ;;
  esac
  if sudo -u shipit test -r "/etc/$SERVICE/.env"; then
    echo "$FAIL shipit 能读取 /etc/$SERVICE/.env"
  else
    echo "$PASS shipit 读不到 /etc/$SERVICE/.env"
  fi
else
  echo "  - /etc/$SERVICE/.env 尚不存在"
fi

check "systemd User=" "$ACCT" "$(systemctl show "$SERVICE" -p User --value 2>/dev/null || echo 不存在)"

echo ""
echo "=== shipit check（自动检查）==="
sudo shipit check
```

---

## 总结

`shipit check` 负责**应用层**验证（配置、单元文件、sudoers 规则、shipit 能否读到服务的
环境变量文件、GitHub 访问权限）。**文件系统层**验证（目录权限、`/etc/<service>` 里的文件）
不会自动强制执行，必须在初始化时手动设置。

最容易被遗漏、危害最大的三项：

1. **Sudoers 规则含通配符** → 通往 root 的路径（R1）
2. **服务仍以 `shipit` 运行，或 `.env` 还在项目目录里** → 一个服务被攻破就能波及全部（R7）；在 `shipit status` 里找 `!`
3. **`.env` 可被其他人读取（权限 `0644`）** → 凭证泄露（R5）

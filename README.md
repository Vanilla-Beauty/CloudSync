# CloudSync

> 本地目录与腾讯云 COS 双向实时同步的轻量命令行工具

CloudSync 采用 **客户端 / 守护进程**分离架构：`cloudsyncd` 常驻后台持续监听文件变更，`cloudsync` 作为控制工具通过 Unix Domain Socket 与守护进程通信。支持类 `.gitignore` 过滤规则、交换区文件自动识别、防抖批量同步、双向冲突解决。

---

## 特性

- **双向同步** — 启动时比较本地与远端时间戳 / ETag，自动上传或下载；冲突时较新的一方获胜，本地副本另存为冲突文件
- **实时同步** — 基于 `fsnotify` 监听文件变更，秒级响应
- **智能过滤** — `.syncignore` 规则（gitignore 语法）+ 自动识别 Office / Vim / Emacs 临时文件
- **防抖 + 批量** — 文件频繁修改时合并处理，避免重复上传
- **增量同步** — SHA-256 哈希 + ETag 对比，未变更文件直接跳过
- **断点恢复** — 挂载列表与元数据持久化，daemon 重启后自动恢复所有同步任务并触发双向初始扫描
- **分片上传** — 文件 ≥ 32 MB 自动切换为并发分片上传
- **速率控制** — 信号量 + 令牌桶双重限流，保护带宽和 COS API 配额
- **暂停 / 恢复** — 随时暂停某个挂载的同步，不影响其他挂载
- **同步统计** — 实时展示每个挂载的上传 / 下载 / 删除 / 错误计数及最后同步时间
- **一次性同步** — 无需守护进程，直接运行 `cloudsync sync` 完成单次双向同步后退出
- **跨平台** — Linux / macOS / Windows，服务注册支持 systemd 和 Windows Service

---

## 快速开始

```bash
# 1. 编译安装
./install.sh --gobin

# 2. 配置 COS 凭据
cloudsync init

# 3. 启动守护进程
cloudsync start

# 4a. 将本地目录同步到 COS
cloudsync mount ~/documents

# 4b. 从 COS 下载已有数据并建立同步
cloudsync download documents/
```

详细步骤见 [GETTING_STARTED.md](./GETTING_STARTED.md)。

---

## 安装

**一键脚本（推荐）**

```bash
git clone https://github.com/your-org/cloudsync.git
cd cloudsync
./install.sh          # 安装到 /usr/local/bin（自动 sudo）
./install.sh --gobin  # 安装到 $GOPATH/bin
./install.sh --dir ~/.local/bin  # 自定义目录
```

**手动编译**

```bash
go build -o cloudsync  ./cmd/cloudsync/
go build -o cloudsyncd ./cmd/cloudsyncd/
```

> `cloudsync` 和 `cloudsyncd` 必须放在同一目录。

**环境要求**：Go 1.21+，腾讯云 COS Bucket 及 API 密钥。

---

## 命令一览

| 命令 | 说明 |
|------|------|
| `cloudsync init` | 配置 COS 凭据，交互列出 bucket 供选择，写入 `config.json` |
| `cloudsync start` | 启动守护进程 |
| `cloudsync stop` | 停止守护进程 |
| `cloudsync status` | 查看守护进程状态与挂载列表（含同步统计） |
| `cloudsync ls` | 列出所有活动挂载 |
| `cloudsync mount <path> [remote]` | 将本地目录挂载到 COS 并开始双向同步 |
| `cloudsync download <remote> [local]` | 将 COS 前缀下载到本地并建立双向同步 |
| `cloudsync unmount <path>` | 停止同步，远端文件保留 |
| `cloudsync delete <path>` | 停止同步并删除所有远端文件（需确认） |
| `cloudsync pause <path>` | 暂停某个挂载的同步 |
| `cloudsync resume <path>` | 恢复某个挂载的同步 |
| `cloudsync sync <path> [remote]` | 一次性双向同步，不依赖守护进程 |

### mount 的三种模式

```bash
# 模式 1：远端前缀 = 目录名（默认）
cloudsync mount ~/a/b/c              # → 默认 bucket/c/

# 模式 2：远端前缀 = 相对 $HOME 的完整路径
cloudsync mount --from-home ~/a/b/c  # → 默认 bucket/a/b/c/

# 模式 3：手动指定远端路径
cloudsync mount ~/a/b/c my/remote    # → 默认 bucket/my/remote/

# 指定其他 bucket
cloudsync mount ~/a/b/c --bucket my-other-bucket
```

### download 的三种模式

```bash
# 模式 1：本地目录 = 当前目录 / 远端前缀的 basename（默认）
cloudsync download projects/         # → ./projects/

# 模式 2：本地目录 = $HOME/<remote>
cloudsync download --to-home a/b/c   # → ~/a/b/c/

# 模式 3：手动指定本地路径
cloudsync download projects/ ~/work  # → ~/work/

# 指定其他 bucket
cloudsync download projects/ --bucket my-other-bucket
```

### 一次性同步（无需守护进程）

```bash
# 双向同步当前目录到 COS，使用 config.json 中的默认 bucket
cloudsync sync ~/documents

# 指定远端前缀
cloudsync sync ~/documents work/docs

# 指定其他 bucket
cloudsync sync ~/documents --bucket backup-bucket
```

完成后打印统计摘要并退出，不会启动任何后台进程。

---

## 双向同步策略

每次挂载（`mount` / `download`）或 daemon 重启时，CloudSync 会对本地与远端做一次**双向初始扫描**：

| 情况 | 行为 |
|------|------|
| 仅本地存在 | 上传到 COS |
| 仅远端存在 | 下载到本地 |
| 两端均存在，有基线记录 | 比对 SHA-256 与 ETag，仅有一方变化则向另一方同步；两方均变化则**较新时间戳获胜**（相同则上传） |
| 两端均存在，无基线记录 | 比对修改时间，差异 > 2s 则较新一方获胜，否则跳过 |

**冲突处理**：当两端均发生变更时，在用远端版本覆盖本地之前，CloudSync 会将本地文件另存为 `文件名 (conflict copy YYYY-MM-DD).扩展名`，防止数据丢失。

---

## 暂停与恢复

```bash
# 暂停某目录的同步（不影响其他挂载，不中断正在进行的传输）
cloudsync pause ~/documents

# 恢复同步
cloudsync resume ~/documents

# 查看暂停状态
cloudsync ls
```

暂停期间产生的本地变更在恢复后**不会**自动补同步，建议恢复后运行 `cloudsync sync` 手动触发一次全量比对。

---

## 同步统计

`cloudsync status` 和 `cloudsync ls` 会显示每个挂载的实时统计：

```
ID          LOCAL PATH                            REMOTE PREFIX         STATUS  LAST SYNC
-----------------------------------------------------------------------------------------------
a3f9c1b2    /home/toni/documents                  documents/            active  2026-03-15 10:30:00
            uploads=12     downloads=3      deletes=1      errors=0
```

---

## 过滤规则

在同步目录下创建 `.syncignore`，语法与 `.gitignore` 完全相同：

```gitignore
# 编译产物
node_modules/
dist/
*.o

# 系统文件
.DS_Store
.git/

# 否定规则（重新包含）
!important.log
```

以下文件**始终自动忽略**（无需配置）：

| 类型 | 示例 |
|------|------|
| Office 锁文件 | `~$document.docx` |
| Vim 交换文件 | `.file.swp`、`file~` |
| Emacs 锁文件 | `.#file`、`#file#` |
| 通用临时文件 | `*.tmp`、`*.temp`、`*.bak`、`*.cache` |

---

## 配置

配置文件路径：`~/.config/cloudsync/config.json`（Windows：`%APPDATA%\cloudsync\config.json`）

```json
{
  "cos": {
    "secret_id":  "AKIDxxxxxxxxx",
    "secret_key": "xxxxxxxxx",
    "bucket":     "my-bucket-1234567890",
    "region":     "ap-guangzhou"
  },
  "performance": {
    "debounce_ms":      2000,
    "batch_interval_ms": 5000,
    "batch_max_size":   100,
    "max_concurrent":   3,
    "qps":              10
  },
  "log": {
    "level":  "info",
    "format": "json"
  }
}
```

环境变量优先级高于配置文件：

```bash
export COS_SECRET_ID="..."
export COS_SECRET_KEY="..."
export COS_BUCKET="..."
export COS_REGION="..."
```

---

## 运行原理

```
文件变更 (fsnotify)
    │
    ├─ 暂停检测（paused → 丢弃）
    ├─ .syncignore 规则过滤
    ├─ 交换区文件检测
    │
    ▼
防抖（2s 窗口）
    │
    ▼
批量收集（5s 或 100 文件）
    │
    ▼
决策：上传 / 下载 / 跳过
    │  比较本地 SHA-256、远端 ETag、修改时间
    ▼
速率限制（3 并发 / 10 QPS）
    │
    ├─ 上传 → 腾讯云 COS（≥32 MB 自动分片）
    └─ 下载 → 本地磁盘（原子写入临时文件后重命名）
```

---

## 文档

| 文档 | 内容 |
|------|------|
| [GETTING_STARTED.md](./GETTING_STARTED.md) | 安装、初始化、挂载与下载的完整操作指南 |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | 代码结构、模块设计与数据流详解 |

---

## 许可证

MIT


---

## 快速开始

```bash
# 1. 编译安装
./install.sh --gobin

# 2. 配置 COS 凭据
cloudsync init

# 3. 启动守护进程
cloudsync start

# 4a. 将本地目录同步到 COS
cloudsync mount ~/documents

# 4b. 从 COS 下载已有数据并建立同步
cloudsync download documents/
```

详细步骤见 [GETTING_STARTED.md](./GETTING_STARTED.md)。

---

## 安装

**一键脚本（推荐）**

```bash
git clone https://github.com/your-org/cloudsync.git
cd cloudsync
./install.sh          # 安装到 /usr/local/bin（自动 sudo）
./install.sh --gobin  # 安装到 $GOPATH/bin
./install.sh --dir ~/.local/bin  # 自定义目录
```

**手动编译**

```bash
go build -o cloudsync  ./cmd/cloudsync/
go build -o cloudsyncd ./cmd/cloudsyncd/
```

> `cloudsync` 和 `cloudsyncd` 必须放在同一目录。

**环境要求**：Go 1.21+，腾讯云 COS Bucket 及 API 密钥。

---

## 命令一览

| 命令 | 说明 |
|------|------|
| `cloudsync init` | 配置 COS 凭据，交互列出 bucket 供选择，写入 `config.json` |
| `cloudsync start` | 启动守护进程 |
| `cloudsync stop` | 停止守护进程 |
| `cloudsync status` | 查看守护进程状态与挂载列表 |
| `cloudsync ls` | 列出所有活动挂载 |
| `cloudsync mount <path> [remote]` | 将本地目录挂载到 COS 并开始双向同步 |
| `cloudsync download <remote> [local]` | 将 COS 前缀下载到本地并建立双向同步 |
| `cloudsync unmount <path>` | 停止同步，远端文件保留 |
| `cloudsync delete <path>` | 停止同步并删除所有远端文件（需确认） |

### mount 的三种模式

```bash
# 模式 1：远端前缀 = 目录名（默认）
cloudsync mount ~/a/b/c              # → 默认 bucket/c/

# 模式 2：远端前缀 = 相对 $HOME 的完整路径
cloudsync mount --from-home ~/a/b/c  # → 默认 bucket/a/b/c/

# 模式 3：手动指定远端路径
cloudsync mount ~/a/b/c my/remote    # → 默认 bucket/my/remote/

# 指定其他 bucket
cloudsync mount ~/a/b/c --bucket my-other-bucket
```

### download 的三种模式

```bash
# 模式 1：本地目录 = 当前目录 / 远端前缀的 basename（默认）
cloudsync download projects/         # → ./projects/

# 模式 2：本地目录 = $HOME/<remote>
cloudsync download --to-home a/b/c   # → ~/a/b/c/

# 模式 3：手动指定本地路径
cloudsync download projects/ ~/work  # → ~/work/

# 指定其他 bucket
cloudsync download projects/ --bucket my-other-bucket
```

---

## 双向同步策略

每次挂载（`mount` / `download`）或 daemon 重启时，CloudSync 会对本地与远端做一次**双向初始扫描**：

| 情况 | 行为 |
|------|------|
| 仅本地存在 | 上传到 COS |
| 仅远端存在 | 下载到本地 |
| 两端均存在，有基线记录 | 比对 SHA-256 与 ETag，仅有一方变化则向另一方同步；两方均变化则**较新时间戳获胜**（相同则上传） |
| 两端均存在，无基线记录 | 比对修改时间，差异 > 2s 则较新一方获胜，否则跳过 |

初始扫描完成后，实时监听阶段仍遵循同样的决策逻辑。

---

## 过滤规则

在同步目录下创建 `.syncignore`，语法与 `.gitignore` 完全相同：

```gitignore
# 编译产物
node_modules/
dist/
*.o

# 系统文件
.DS_Store
.git/

# 否定规则（重新包含）
!important.log
```

以下文件**始终自动忽略**（无需配置）：

| 类型 | 示例 |
|------|------|
| Office 锁文件 | `~$document.docx` |
| Vim 交换文件 | `.file.swp`、`file~` |
| Emacs 锁文件 | `.#file`、`#file#` |
| 通用临时文件 | `*.tmp`、`*.temp`、`*.bak`、`*.cache` |

---

## 配置

配置文件路径：`~/.config/cloudsync/config.json`（Windows：`%APPDATA%\cloudsync\config.json`）

```json
{
  "cos": {
    "secret_id":  "AKIDxxxxxxxxx",
    "secret_key": "xxxxxxxxx",
    "bucket":     "my-bucket-1234567890",
    "region":     "ap-guangzhou"
  },
  "performance": {
    "debounce_ms":      2000,
    "batch_interval_ms": 5000,
    "batch_max_size":   100,
    "max_concurrent":   3,
    "qps":              10
  },
  "log": {
    "level":  "info",
    "format": "json"
  }
}
```

环境变量优先级高于配置文件：

```bash
export COS_SECRET_ID="..."
export COS_SECRET_KEY="..."
export COS_BUCKET="..."
export COS_REGION="..."
```

---

## 运行原理

```
文件变更 (fsnotify)
    │
    ├─ .syncignore 规则过滤
    ├─ 交换区文件检测
    │
    ▼
防抖（2s 窗口）
    │
    ▼
批量收集（5s 或 100 文件）
    │
    ▼
决策：上传 / 下载 / 跳过
    │  比较本地 SHA-256、远端 ETag、修改时间
    ▼
速率限制（3 并发 / 10 QPS）
    │
    ├─ 上传 → 腾讯云 COS
    └─ 下载 → 本地磁盘（原子写入临时文件后重命名）
```

---

## 文档

| 文档 | 内容 |
|------|------|
| [GETTING_STARTED.md](./GETTING_STARTED.md) | 安装、初始化、挂载与下载的完整操作指南 |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | 代码结构、模块设计与数据流详解 |

---

## 许可证

MIT

# CloudSync

> 将本地目录实时同步到腾讯云 COS 的轻量命令行工具

CloudSync 采用 **客户端 / 守护进程**分离架构：`cloudsyncd` 常驻后台持续监听文件变更，`cloudsync` 作为控制工具通过 Unix Domain Socket 与守护进程通信。支持类 `.gitignore` 过滤规则、交换区文件自动识别、防抖批量上传。

---

## 特性

- **实时同步** — 基于 `fsnotify` 监听文件变更，秒级响应
- **智能过滤** — `.syncignore` 规则（gitignore 语法）+ 自动识别 Office / Vim / Emacs 临时文件
- **防抖 + 批量** — 文件频繁修改时合并处理，避免重复上传
- **增量同步** — SHA-256 哈希对比，未变更文件直接跳过
- **断点恢复** — 挂载列表持久化，daemon 重启后自动恢复所有同步任务
- **速率控制** — 信号量 + 令牌桶双重限流，保护带宽和 COS API 配额
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

# 4. 开始同步
cloudsync mount ~/documents
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
| `cloudsync init` | 配置 COS 凭据，写入 `~/.config/cloudsync/config.json` |
| `cloudsync start` | 启动守护进程 |
| `cloudsync stop` | 停止守护进程 |
| `cloudsync status` | 查看守护进程状态与挂载列表 |
| `cloudsync ls` | 列出所有活动挂载 |
| `cloudsync mount <path>` | 开始同步目录（三种模式，见下方） |
| `cloudsync unmount <path>` | 停止同步，远端文件保留 |
| `cloudsync delete <path>` | 停止同步并删除所有远端文件（需确认） |

### mount 的三种模式

```bash
# 模式 1：远端前缀 = 目录名（默认）
cloudsync mount ~/a/b/c              # → bucket/c/

# 模式 2：远端前缀 = 相对 $HOME 的完整路径
cloudsync mount --from-home ~/a/b/c  # → bucket/a/b/c/

# 模式 3：手动指定远端路径
cloudsync mount ~/a/b/c my/remote    # → bucket/my/remote/
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
SHA-256 增量对比（跳过未变更文件）
    │
    ▼
速率限制（3 并发 / 10 QPS）
    │
    ▼
上传到腾讯云 COS
```

---

## 文档

| 文档 | 内容 |
|------|------|
| [GETTING_STARTED.md](./GETTING_STARTED.md) | 安装、初始化、挂载目录的完整操作指南 |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | 代码结构、模块设计与数据流详解 |

---

## 许可证

MIT

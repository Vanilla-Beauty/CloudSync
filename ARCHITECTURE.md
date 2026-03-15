# CloudSync 架构文档

> 本文档描述当前已实现的代码结构与运行逻辑，与 `CLAUDE.md` 中的设计方案保持对照。
> 最后更新：2026-03-15

---

## 一、整体定位

CloudSync 是一个将本地目录实时同步到腾讯云 COS 的工具，采用 **Client / Daemon 分离架构**：

| 角色 | 二进制 | 职责 |
|------|--------|------|
| 控制工具 | `cloudsync` | 纯 CLI，所有操作通过 IPC 下发给 daemon |
| 守护进程 | `cloudsyncd` | 常驻后台，管理文件监听、过滤、同步 |

两者通过 **Unix Domain Socket + HTTP REST** 通信，无需第三方 IPC 库。`cloudsyncd` 的生命周期由 `kardianos/service` 管理，可注册为 systemd service（Linux）或 Windows Service。

---

## 二、目录结构

```
cloudsync/
├── cmd/
│   ├── cloudsync/main.go        # CLI 入口：init/start/stop/status/mount/unmount/delete/ls
│   └── cloudsyncd/main.go       # Daemon 入口：kardianos/service.Run()
├── internal/
│   ├── ipc/
│   │   └── socket.go            # 跨平台路径工具 + 共享数据类型（MountRecord）
│   ├── config/
│   │   └── config.go            # JSON 配置读写（替代原 viper/YAML）
│   ├── daemon/
│   │   ├── types.go             # IPC 请求/响应结构体
│   │   ├── daemon.go            # Program 结构体，kardianos/service 集成，启动序列
│   │   └── mountmanager.go      # 挂载生命周期管理 + mounts.json 持久化
│   ├── apiserver/
│   │   ├── server.go            # Unix Socket HTTP 服务端
│   │   └── handlers.go          # REST handler（/status、/mounts）
│   ├── apiclient/
│   │   └── client.go            # Unix Socket HTTP 客户端
│   ├── filter/
│   │   ├── ignore.go            # .syncignore 规则解析与匹配
│   │   └── swap.go              # 交换区/临时文件检测
│   ├── watcher/
│   │   ├── watcher.go           # fsnotify 监听核心，递归目录，事件分发
│   │   ├── debounce.go          # 每文件独立计时器防抖
│   │   └── batch.go             # 去重 map + ticker 批量收集
│   ├── storage/
│   │   ├── cos.go               # COS SDK 封装，指数退避重试
│   │   ├── sync.go              # Syncer：SyncFiles() / SyncDirectory()
│   │   └── metadata.go          # 内存哈希/状态缓存 + HashFile()
│   └── limiter/
│       └── rate.go              # 信号量并发控制 + 令牌桶 QPS 限制
└── pkg/
    └── utils/utils.go           # 路径工具（FileExists/DirExists/RelPath/WalkDirs）
```

---

## 三、持久化文件布局

```
~/.config/cloudsync/          （Linux/macOS，Windows 为 %APPDATA%\cloudsync\）
├── config.json               # COS 凭据 + 性能参数，由 cloudsync init 写入
├── mounts.json               # 挂载列表，仅 daemon 写入（原子 rename）
├── cloudsyncd.sock           # IPC Unix Domain Socket
├── cloudsyncd.pid            # daemon 进程 PID
└── cloudsyncd.log            # daemon 运行日志
```

**config.json 结构**：
```json
{
  "cos":         { "secret_id":"", "secret_key":"", "bucket":"", "region":"ap-guangzhou" },
  "performance": { "debounce_ms":2000, "batch_interval_ms":5000,
                   "batch_max_size":100, "max_concurrent":3, "qps":10 },
  "log":         { "level":"info", "format":"json" }
}
```

**mounts.json 结构**：
```json
{
  "mounts": [
    { "id":"a3f9c1b2", "local_path":"/home/toni/docs",
      "remote_prefix":"docs/", "added_at":"2026-03-15T10:00:00Z" }
  ]
}
```

---

## 四、IPC 协议

**Socket 路径**：`~/.config/cloudsync/cloudsyncd.sock`

| Method | Path | 功能 |
|--------|------|------|
| GET | `/status` | daemon 健康检查，返回 pid / 版本 / 挂载数 |
| GET | `/mounts` | 列出所有活动挂载 |
| POST | `/mounts` | 新增挂载 `{"local_path":"...","remote_prefix":"..."}` |
| DELETE | `/mounts` | 移除挂载 `{"local_path":"...","delete_remote":false}` |

所有响应为 `application/json`。错误格式：`{"error":"message"}`。

客户端 (`apiclient.Client`) 使用自定义 `http.Transport`，将 `DialContext` 替换为 `net.Dial("unix", socketPath)`，Host 固定为 `cloudsyncd`，对上层完全透明。

---

## 五、模块详解

### 5.1 ipc/socket.go — 共享基础

**两个职责合一**：

1. **路径工具**：`ConfigDir()` 按平台返回配置目录，衍生出 `SocketPath()`、`ConfigFilePath()`、`MountsFilePath()`、`PIDFilePath()`、`LogFilePath()`。

2. **共享类型**：`MountRecord` 和 `MountsFile` 定义在此，供 `daemon`、`apiserver` 双方引用，避免循环依赖。
   ```
   daemon ──引用──▶ ipc.MountRecord
   apiserver ──引用──▶ ipc.MountRecord
   daemon ──引用──▶ apiserver（通过 MountManagerAPI 接口）
   ```

### 5.2 config/config.go — 配置管理

- 完全基于 `encoding/json`，去除 viper 依赖。
- `DefaultConfig()` 提供合理默认值。
- `Load(path)` 先读 JSON 文件，再用环境变量覆盖（`COS_SECRET_ID` / `COS_SECRET_KEY` / `COS_BUCKET` / `COS_REGION`）。文件不存在时返回友好提示引导用户执行 `cloudsync init`。
- `Save(path, cfg)` 以 `0600` 权限写入，保护凭据。

### 5.3 daemon/daemon.go — 守护进程核心

实现 `kardianos/service.Interface`（`Start` / `Stop` 两个方法）。

**`Program.Start()` 调用 `run()` goroutine，启动序列如下**：

```
MkdirAll(configDir)
  │
  ▼
config.Load(config.json)
  │
  ▼
buildDaemonLogger()        # JSON 格式，写 cloudsyncd.log + stdout
  │
  ▼
storage.NewCOSClient()     # 验证 bucket/secret 非空
  │
  ▼
storage.NewMetadataStore() # 内存哈希表
  │
  ▼
limiter.NewRateLimiter()   # 信号量 + 令牌桶
  │
  ▼
daemon.NewMountManager()
  │
  ▼
MountManager.LoadSaved()   # 读 mounts.json，重建所有 watcher
  │
  ▼
apiserver.Server.Start()   # os.Remove(sock) + net.Listen("unix") + http.Serve
  │
  ▼
写 cloudsyncd.pid
  │
  ▼
<-stopCh                   # 阻塞，等待 Stop() 关闭
```

**`Program.Stop()`**：关闭 `stopCh`，依次调用 `MountManager.StopAll()`、`apiServer.Stop()`、`logger.Sync()`，最后删除 PID 文件。

### 5.4 daemon/mountmanager.go — 挂载管理

核心数据结构：
```go
entries map[string]*watcherEntry   // key: MountRecord.ID
```

每个 `watcherEntry` 持有一个 `ipc.MountRecord`（元数据）和一个 `*watcher.SyncWatcher`（运行实例）。

**`Add(localPath, remotePrefix)`**：
1. 生成 8 位随机十六进制 ID
2. 调用 `startWatcher(rec)` 创建并启动 fsnotify watcher
3. 在 goroutine 中调用 `Syncer.SyncDirectory()` 做初始全量扫描
4. 原子写入 `mounts.json`（写 `.tmp` 再 `os.Rename`，Windows 先 `os.Remove`）
5. 若 save 失败则回滚（停止刚创建的 watcher，从 map 删除）

**`Remove(localPath, deleteRemote)`**：
1. 在 `entries` 中按 `LocalPath` 查找
2. 从 map 删除，调用 `watcher.Stop()`
3. 若 `deleteRemote=true`，调用 `COS.List()` + `COS.Delete()` 清除远程对象
4. 原子写入更新后的 `mounts.json`

**`LoadSaved()`**：读取 `mounts.json`，对每条记录调用 `startWatcher()`，跳过失败项（日志警告）。

**`.syncignore` 路径约定**：每个挂载目录独立，路径为 `filepath.Join(record.LocalPath, ".syncignore")`，文件不存在时 `LoadIgnoreRules` 返回空规则集（不报错）。

### 5.5 apiserver — HTTP over Unix Socket

`server.go` 负责：
- 启动前执行 `os.Remove(socketPath)` 清理崩溃残留
- `net.Listen("unix", socketPath)` 监听
- 在 goroutine 中 `http.Serve`

`handlers.go` 定义 `MountManagerAPI` 接口（使用 `ipc.MountRecord`），`MountManager` 自然满足该接口，无需适配器。路由分发：
- `/status` → `GET` only
- `/mounts` → 按 HTTP method 分发到 `listMounts` / `addMount` / `removeMount`

### 5.6 apiclient — 客户端

使用自定义 Transport 拨号，对失败统一返回 `"daemon is not running — use 'cloudsync start'"` 的友好错误。提供：
- `Ping()` — 快速存活检测
- `Status()` — 获取状态
- `ListMounts()` / `AddMount()` / `RemoveMount()` — 挂载操作

### 5.7 filter/ignore.go — 规则引擎

将 gitignore 风格的 glob 模式编译为正则表达式：

| glob 语法 | 转换规则 |
|----------|---------|
| `*` | `[^/]*`（不跨目录） |
| `**` | `.*`（跨目录） |
| `?` | `[^/]` |
| `/` 开头 | `^` 锚定根目录 |
| `/` 结尾 | `(/.*)?$`（目录及其内容） |
| `!` 开头 | negate=true，重新包含 |

`Match(path)` 按顺序应用所有规则，后面的规则可覆盖前面的（gitignore 语义）。同时匹配完整路径和 basename，增强跨平台兼容性。

### 5.8 filter/swap.go — 交换区检测

硬编码三层检测（均大小写不敏感）：

| 层级 | 规则 |
|------|------|
| 前缀 | `~$`（Office 锁文件）、`.#`（Emacs 锁文件）、`#`（Emacs 自动保存） |
| 后缀 | `~`（Vim/Emacs 备份）、`.tmp`、`.swp`、`.swo`、`.temp`、`.bak` |
| 扩展名 | `.tmp`、`.swp`、`.swo`、`.temp`、`.bak`、`.cache` |

### 5.9 watcher/watcher.go — 文件监听核心

**构造（`New()`）**：按依赖顺序创建组件，用闭包串联数据流：

```
fsnotify.Event
    │
    ▼
handleEvent()
    │ shouldIgnore()：IgnoreRules.Match + SwapDetector.IsSwapFile
    │ 新建目录时递归注册 Watch
    ▼
Debouncer.Trigger(path)
    │ 计时器到期（默认 2s）
    ▼
Batcher.Add(path)
    │ ticker 到期（默认 5s）或达到 maxSize（默认 100）
    ▼
processBatch(paths) → Syncer.SyncFiles(ctx, paths)
```

`addPathRecursive()` 使用 `filepath.Walk` 递归注册目录，跳过符号链接。

### 5.10 watcher/debounce.go — 防抖

每个文件路径维护独立 `time.Timer`，`Trigger()` 到达时：
- 若已有计时器：`Stop()` + drain channel + `Reset(delay)`
- 若没有：创建新 `time.AfterFunc`

计时器到期后从 map 删除自身并调用 callback（进入 Batcher）。`Close()` 取消所有待触发计时器。

### 5.11 watcher/batch.go — 批量收集

内部使用 `map[string]struct{}` 存储路径（天然去重），有两个触发时机：
- **数量触发**：`Add()` 时 `len(batch) >= maxSize` 立即 `Flush()`
- **时间触发**：后台 goroutine 的 `ticker.C` 到期时 `Flush()`

`Flush()` 在加锁内做 snapshot 并清空 map，然后在锁外执行 callback，避免长时间持锁。

### 5.12 storage/cos.go — COS 操作

封装腾讯云 cos-go-sdk-v5，统一通过 `withRetry()` 执行最多 3 次，退避策略：

```
第 1 次失败 → 等 500ms
第 2 次失败 → 等 1000ms
第 3 次失败 → 返回错误（携带 "after 3 retries:" 前缀）
```

支持 `context.Done()` 中断等待。提供 `Upload` / `Delete` / `Exists` / `List`。

### 5.13 storage/sync.go — 同步编排

`Syncer` 持有 COS 客户端、元数据缓存、限速器，绑定到特定 `(localRoot, remotePrefix)` 对。

**`SyncFiles(ctx, paths)`**：并发处理（每个文件一个 goroutine），等待全部完成。

**单文件同步 `syncOne()`**：
1. `os.Stat()` 检查文件是否存在，不存在则触发远端删除
2. `HashFile()` 计算 SHA-256
3. 查 `MetadataStore`，哈希未变则跳过（增量同步）
4. `RateLimiter.Acquire()` 获取令牌 + 并发槽
5. `COSClient.Upload()`
6. 成功后更新 `MetadataStore`

**`SyncDirectory(ctx)`**：`filepath.Walk` 遍历 localRoot 下所有文件，交给 `SyncFiles` 批量处理。用于 daemon 重启或新挂载时的初始全量扫描。

**remoteKey 计算**：`filepath.Rel(localRoot, localPath)` → `filepath.ToSlash` → 去掉 `./` → 拼接 remotePrefix。

### 5.14 storage/metadata.go — 元数据缓存

**当前实现为纯内存**（daemon 重启后数据丢失，下次启动会重新全量计算哈希并上传有变化的文件）。

```go
hashes map[string]string      // localPath → sha256hex
status map[string]*SyncStatus // localPath → {LastSyncedAt, RemoteKey, Hash}
```

`HashFile()` 流式读取文件计算 SHA-256，不将文件全部加载入内存。

### 5.15 limiter/rate.go — 双重限流

两个机制叠加：
- **信号量** `chan struct{}{capacity=maxConcurrent}`：控制同时进行的 COS 请求数
- **令牌桶** `golang.org/x/time/rate.Limiter`：控制每秒请求数（QPS）

`Acquire()` 先等令牌桶（`limiter.Wait`），再抢信号量 slot；`Release()` 释放 slot。两者都支持 `context.Done()` 取消。

---

## 六、完整事件处理流程

```
本地文件变更（fsnotify.Event）
        │
        ▼
    handleEvent()
        │
        ├── 新建目录？ → addPathRecursive() 递归注册 Watch → 返回
        │
        ├── shouldIgnore()
        │       ├── IgnoreRules.Match(relPath)  ← .syncignore 规则
        │       └── SwapDetector.IsSwapFile()   ← 交换区检测
        │   命中 → 丢弃
        │
        ▼
    Debouncer.Trigger(path)
        │  2s 内无新事件则到期
        ▼
    Batcher.Add(path)         ← 去重 map
        │  5s 或 100 文件触发
        ▼
    processBatch(paths)
        │
        ▼
    Syncer.SyncFiles(ctx, paths)
        │  每文件并发 goroutine
        ▼
    syncOne(path)
        │
        ├── 文件不存在 → COSClient.Delete(remoteKey)
        │
        ├── HashFile(path) → 与 MetadataStore 比对
        │   哈希未变 → 跳过
        │
        ├── RateLimiter.Acquire()   ← 信号量 + 令牌桶
        ├── COSClient.Upload(path, remoteKey)
        │       └── withRetry(最多3次，指数退避)
        └── MetadataStore.SetFileHash(path, hash)
```

---

## 七、CLI 命令与 Daemon 交互

```
cloudsync init
    写 ~/.config/cloudsync/config.json（交互式提示缺省参数）

cloudsync start
    1. apiclient.Ping()  成功 → "already running"
    2. 失败 → 同目录找 cloudsyncd 二进制
             → service.Install() + service.Start()
             → 失败则 exec.Command(cloudsyncd).Start()（直接启动，非 service）
             → 轮询 socket 最多 5s

cloudsync stop
    service.Stop()

cloudsync status
    GET /status + GET /mounts → 格式化输出表格

cloudsync mount <path> [--prefix <remote-prefix>]
    filepath.Abs → POST /mounts

cloudsync unmount <path>
    DELETE /mounts {delete_remote: false}

cloudsync delete <path>
    交互式确认 [y/N] → DELETE /mounts {delete_remote: true}

cloudsync ls
    GET /mounts → 表格输出
```

---

## 八、依赖一览

| 包 | 用途 |
|----|------|
| `github.com/fsnotify/fsnotify` | 跨平台文件系统事件 |
| `github.com/tencentyun/cos-go-sdk-v5` | 腾讯云 COS 官方 SDK |
| `github.com/kardianos/service` | 跨平台守护进程/服务管理 |
| `github.com/spf13/cobra` | CLI 框架 |
| `go.uber.org/zap` | 高性能结构化日志 |
| `golang.org/x/time/rate` | 令牌桶限流 |

> viper 已移除（配置改用 encoding/json），mattn/go-sqlite3 尚未引入（元数据仍为内存实现）。

---

## 九、已知局限与后续规划

| 项目 | 当前状态 | 后续方向 |
|------|---------|---------|
| 元数据持久化 | 内存，重启丢失 | V3：SQLite（`mattn/go-sqlite3`） |
| 同步方向 | 仅上行（本地→COS） | V5：双向同步，支持 Download |
| 配置热加载 | 不支持，需重启 daemon | 监听 config.json 变更信号 |
| 系统托盘 | 未实现 | 独立 UI 进程，通过同一 socket 通信 |
| 文件权限同步 | 不同步 | 可作为 COS Object metadata 扩展 |
| 符号链接 | 跳过（不追踪） | 可配置化 |

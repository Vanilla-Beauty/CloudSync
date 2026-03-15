# Getting Started with CloudSync

CloudSync 是一个将本地目录与腾讯云 COS **双向**实时同步的命令行工具，采用守护进程架构，支持 `.syncignore` 过滤规则和智能防抖批量同步。

---

## 目录

- [前置条件](#前置条件)
- [编译安装](#编译安装)
- [第一步：初始化配置](#第一步初始化配置)
- [第二步：启动守护进程](#第二步启动守护进程)
- [第三步：建立同步](#第三步建立同步)
  - [方式 A：从本地发起（mount）](#方式-a从本地发起mount)
  - [方式 B：从远端发起（download）](#方式-b从远端发起download)
- [日常使用](#日常使用)
  - [查看状态与同步统计](#查看状态与同步统计)
  - [暂停与恢复](#暂停与恢复)
  - [一次性同步（无需守护进程）](#一次性同步无需守护进程)
  - [挂载多个目录](#挂载多个目录)
  - [触发同步的时机](#触发同步的时机)
  - [双向同步策略](#双向同步策略)
  - [冲突处理](#冲突处理)
  - [测试同步是否正常](#测试同步是否正常)
- [过滤规则（.syncignore）](#过滤规则syncignore)
- [停止与卸载](#停止与卸载)
- [命令速查](#命令速查)
- [常见问题](#常见问题)

---

## 前置条件

| 项目 | 要求 |
|------|------|
| Go | 1.21 或更高版本 |
| 操作系统 | Linux / macOS / Windows |
| 腾讯云账号 | 已开通 COS 服务，准备好 SecretId 和 SecretKey |

获取腾讯云 COS 凭据：登录[腾讯云控制台](https://console.cloud.tencent.com/cam/capi) → 访问管理 → API 密钥管理。

---

## 编译安装

```bash
# 克隆代码
git clone https://github.com/your-org/cloudsync.git
cd cloudsync

# 编译两个二进制文件，输出到同一目录（必须放在一起）
go build -o cloudsync  ./cmd/cloudsync/
go build -o cloudsyncd ./cmd/cloudsyncd/

# 可选：安装到 PATH
sudo mv cloudsync cloudsyncd /usr/local/bin/
```

> **注意**：`cloudsync` 启动守护进程时会在自身所在目录寻找 `cloudsyncd`，两个文件必须放在同一目录。

---

## 第一步：初始化配置

运行 `init` 命令，输入 SecretId 和 SecretKey 后，CloudSync 会自动拉取账号下所有可用的 bucket 并以列表形式展示，供你选择一个作为默认 bucket：

```bash
./cloudsync init
```

```
COS SecretId: your-secret-id
COS SecretKey: your-secret-key
Fetching bucket list...

  [1] my-bucket-1234567890  (ap-guangzhou)  created 2025-01-01T00:00:00Z
  [2] backup-bucket-0987    (ap-shanghai)   created 2025-06-15T00:00:00Z
  [3] media-bucket-5678     (ap-beijing)    created 2026-01-10T00:00:00Z

Select default bucket [1]: 1
Selected: my-bucket-1234567890 (ap-guangzhou)
Config written to /home/toni/.config/cloudsync/config.json
```

也可以通过 flag 跳过交互，适合脚本环境：

```bash
# 提供 --bucket 可跳过列表选择（region 从 config 默认值 ap-guangzhou 取）
./cloudsync init \
  --secret-id  "AKIDxxx" \
  --secret-key "xxx" \
  --bucket     "my-bucket-1234567890"
```

或者通过环境变量（优先级高于配置文件）：

```bash
export COS_SECRET_ID="AKIDxxx"
export COS_SECRET_KEY="xxx"
export COS_BUCKET="my-bucket-1234567890"
export COS_REGION="ap-guangzhou"
```

配置文件保存在 `~/.config/cloudsync/config.json`（Windows：`%APPDATA%\cloudsync\config.json`），权限为 `0600`，仅当前用户可读。

---

## 第二步：启动守护进程

```bash
./cloudsync start
```

```
cloudsyncd started
```

首次启动会尝试将 `cloudsyncd` 注册为系统服务（Linux 使用 systemd，Windows 使用 Windows Service）。如果没有足够权限，会回退到直接启动后台进程。

确认守护进程正在运行：

```bash
./cloudsync status
```

```
Daemon PID:   12345
Version:      1.0.0
Mounts:       0

No active mounts.
```

---

## 第三步：建立同步

### 方式 A：从本地发起（mount）

将本地目录挂载到 COS，立即触发双向初始扫描（本地多余的文件上传，COS 多余的文件下载）：

```bash
# 模式 1：远端前缀 = 目录名（默认）
./cloudsync mount ~/documents          # → 默认 bucket/documents/

# 模式 2：远端前缀 = 相对 $HOME 的完整路径
./cloudsync mount --from-home ~/a/b/c  # → 默认 bucket/a/b/c/

# 模式 3：手动指定远端路径
./cloudsync mount ~/documents work/docs  # → 默认 bucket/work/docs/

# 使用其他 bucket（如多项目场景）
./cloudsync mount ~/documents --bucket project-b-bucket
```

```
Mounted: /home/toni/documents → documents/ (id: a3f9c1b2)
```

### 方式 B：从远端发起（download）

已在 COS 上有数据，想在本地建立镜像并持续双向同步：

```bash
# 模式 1：本地 = 当前目录 / 远端前缀 basename
./cloudsync download documents/        # → ./documents/

# 模式 2：本地 = $HOME/<remote>
./cloudsync download --to-home a/b/c   # → ~/a/b/c/

# 模式 3：手动指定本地路径
./cloudsync download documents/ ~/work # → ~/work/

# 使用其他 bucket
./cloudsync download documents/ --bucket project-b-bucket
```

```
Downloading: documents/ → /home/toni/documents (id: b7e2a4c1)
Sync established. Remote files will be downloaded in the background.
```

两种方式最终效果相同：都会建立一条双向同步挂载关系，并在后台完成初始扫描。

验证挂载已生效：

```bash
./cloudsync ls
```

```
ID          LOCAL PATH                            REMOTE PREFIX         STATUS  LAST SYNC
-----------------------------------------------------------------------------------------------
a3f9c1b2    /home/toni/documents                  documents/            active  2026-03-15 10:00:00
            uploads=0      downloads=5      deletes=0      errors=0
```

---

## 日常使用

### 查看状态与同步统计

```bash
./cloudsync status    # 守护进程信息 + 所有挂载列表（含统计）
./cloudsync ls        # 仅列出挂载列表
```

输出示例：

```
Daemon PID:   12345
Version:      1.0.0
Mounts:       2

ID          LOCAL PATH                            REMOTE PREFIX         STATUS  LAST SYNC
-----------------------------------------------------------------------------------------------
a3f9c1b2    /home/toni/documents                  documents/            active  2026-03-15 10:30:05
            uploads=12     downloads=3      deletes=1      errors=0
b7e2a4c1    /home/toni/pictures                   pictures/             paused  2026-03-15 09:15:22
            uploads=50     downloads=0      deletes=2      errors=1
```

### 暂停与恢复

```bash
# 暂停某个挂载（不影响其他挂载，不中断正在进行的传输）
./cloudsync pause ~/documents

# 恢复同步
./cloudsync resume ~/documents
```

暂停期间的本地变更**不会**被自动补同步。恢复后如需立即同步当前差异，可运行：

```bash
./cloudsync sync ~/documents
```

### 一次性同步（无需守护进程）

`sync` 命令直接读取 `config.json`，执行一次完整的双向同步后退出，不需要 daemon 运行：

```bash
# 双向同步，远端前缀 = 目录名
./cloudsync sync ~/documents

# 指定远端前缀
./cloudsync sync ~/documents work/docs

# 指定其他 bucket
./cloudsync sync ~/documents --bucket backup-bucket
```

```
Syncing /home/toni/documents ↔ my-bucket-1234567890/documents/ ...
Done. uploads=3  downloads=1  deletes=0  errors=0
```

适用于 cron job、CI/CD 管道或偶发性备份场景。

### 挂载多个目录

```bash
# 同一账号下不同 bucket
./cloudsync mount ~/projects  dev/projects
./cloudsync mount ~/pictures  media/pictures  --bucket media-bucket
./cloudsync download notes/   ~/notes         --bucket notes-bucket
```

### 触发同步的时机

CloudSync 使用**防抖 + 批量**策略，不是每次保存立刻上传：

1. 文件变更后开始 **2 秒**倒计时
2. 倒计时期间再次变更则重置计时器（防抖）
3. 倒计时结束，文件进入批量队列
4. 批量队列每 **5 秒**或累计 **100 个文件**时统一处理

因此保存文件后约 **2～7 秒**内完成同步，这是设计预期行为。

### 双向同步策略

| 情况 | 行为 |
|------|------|
| 仅本地存在 | 上传到 COS |
| 仅远端存在 | 下载到本地 |
| 两端均存在，有上次同步基线 | 比对 SHA-256 与 ETag：仅本地变化→上传；仅远端变化→下载；两端均变化→**较新时间戳获胜**（相同则上传） |
| 两端均存在，无基线（首次） | 比对修改时间，差异 > 2s 则较新一方获胜，否则跳过 |

### 冲突处理

当两端**均发生变更**时（有基线记录，且本地 SHA-256 和远端 ETag 与基线均不同），CloudSync 会：

1. 将当前本地文件另存为 `文件名 (conflict copy YYYY-MM-DD).扩展名`
2. 用远端较新版本覆盖本地

例如 `report.docx` 在 2026-03-15 发生冲突，会产生 `report (conflict copy 2026-03-15).docx`。

### 测试同步是否正常

```bash
mkdir /tmp/synctest
./cloudsync mount /tmp/synctest test/

echo "hello cloudsync" > /tmp/synctest/hello.txt
# 等待约 3 秒...
# 登录腾讯云 COS 控制台确认 test/hello.txt 已上传

# 测试反向同步：在 COS 控制台上传一个新文件到 test/ 前缀
# 然后重新挂载（或重启 daemon），确认文件出现在 /tmp/synctest/
```

---

## 过滤规则（.syncignore）

在被同步的目录下创建 `.syncignore` 文件，语法与 `.gitignore` 完全一致。

```bash
cat > ~/documents/.syncignore << 'EOF'
# 临时文件
*.tmp
*.temp
*.bak
*.log

# 编译产物
node_modules/
dist/
build/
*.exe
*.o

# 系统文件
.DS_Store
Thumbs.db
.git/

# 否定规则：即使匹配上面的规则，这个文件也要同步
!important.log
EOF
```

**规则语法速查**：

| 写法 | 含义 |
|------|------|
| `*.tmp` | 忽略所有 .tmp 文件 |
| `temp/` | 忽略 temp 目录及其全部内容 |
| `/build` | 仅忽略根目录下的 build（不影响子目录） |
| `**/logs/` | 忽略任意层级的 logs 目录 |
| `!keep.log` | 重新包含 keep.log（覆盖前面的忽略规则） |

**自动忽略**：无论 `.syncignore` 如何配置，以下文件始终被忽略（交换区检测）：

- 前缀：`~$`（Office 锁）、`.#`（Emacs 锁）
- 后缀：`~`、`.tmp`、`.swp`、`.swo`、`.temp`、`.bak`、`.cache`

---

## 停止与卸载

### 停止同步（保留远程文件）

```bash
# 取消某个目录的同步，远程 COS 文件不受影响
./cloudsync unmount ~/documents

# 停止守护进程
./cloudsync stop
```

### 删除远程文件并停止同步

```bash
# 会弹出确认提示
./cloudsync delete ~/documents
```

```
This will delete all remote files for /home/toni/documents.
Continue? [y/N] y
Deleted remote files and unmounted: /home/toni/documents
```

### 完整重置

```bash
./cloudsync stop
rm ~/.config/cloudsync/config.json
rm ~/.config/cloudsync/mounts.json
rm ~/.config/cloudsync/metadata.db
```

---

## 命令速查

```
cloudsync init [--secret-id STR] [--secret-key STR] [--bucket STR]
    输入凭据后列出所有 bucket 供交互选择，写入 config.json
    --bucket 可直接指定跳过选择

cloudsync start
    启动守护进程（已运行则提示 already running）

cloudsync stop
    停止守护进程

cloudsync status
    显示守护进程状态和所有挂载目录（含同步统计）

cloudsync mount <path> [remote] [--from-home] [--bucket BUCKET]
    将本地目录挂载到 COS，建立双向同步
    --from-home   远端前缀相对 $HOME
    --bucket      覆盖默认 bucket

cloudsync download <remote> [local] [--to-home] [--bucket BUCKET]
    将 COS 前缀下载到本地，建立双向同步
    --to-home     本地目录放在 $HOME 下
    --bucket      覆盖默认 bucket

cloudsync unmount <path>
    停止同步，远程文件保留

cloudsync delete <path>
    停止同步并删除所有远程文件（需交互确认）

cloudsync ls
    列出所有当前活动的挂载（含统计）

cloudsync pause <path>
    暂停某个挂载的同步

cloudsync resume <path>
    恢复某个暂停的挂载

cloudsync sync <path> [remote] [--bucket BUCKET]
    一次性双向同步，不依赖守护进程，完成后退出
    --bucket      覆盖默认 bucket
```

---

## 常见问题

**Q：运行 `cloudsync start` 提示 "cloudsyncd binary not found"**

确保 `cloudsync` 和 `cloudsyncd` 在同一目录，且都有执行权限：
```bash
ls -la /usr/local/bin/cloudsync*
chmod +x /usr/local/bin/cloudsyncd
```

**Q：运行任何命令提示 "daemon is not running"**

守护进程未启动，执行 `cloudsync start`。如果只是想做一次同步，可以用 `cloudsync sync <path>` 代替。

**Q：文件已保存但 COS 上没有出现**

1. 检查 `.syncignore` 规则是否误过滤了该文件
2. 检查文件名是否触发了交换区检测（以 `~$`、`.#` 开头，或以 `.tmp`、`.swp` 等结尾）
3. 查看 daemon 日志确认有无错误：`tail -f ~/.config/cloudsync/cloudsyncd.log`
4. 确认 COS 凭据和 Bucket 名称正确：`cat ~/.config/cloudsync/config.json`
5. 检查该挂载是否处于暂停状态：`cloudsync ls`

**Q：`download` 和 `mount` 有什么区别**

行为完全相同，只是入口方向不同。`mount` 以本地路径为主参数，适合"我有本地文件，想上传到 COS"的场景；`download` 以远端前缀为主参数，适合"COS 上已有数据，想在本地建立镜像"的场景。两者建立的挂载关系均为双向同步。

**Q：两端同时修改同一文件会怎样**

CloudSync 以**较新的修改时间**为准，并将被覆盖的本地文件另存为冲突副本（`文件名 (conflict copy YYYY-MM-DD).扩展名`）。若时间差在 2 秒以内视为相同，此时上传本地版本。

**Q：`cloudsync init` 后凭据或 bucket 如何更新**

直接重新运行 `cloudsync init`，会再次列出 bucket 供选择并覆盖旧配置。修改后需要重启 daemon：
```bash
cloudsync stop && cloudsync start
```

**Q：daemon 重启后之前挂载的目录还在吗**

是的。挂载列表保存在 `~/.config/cloudsync/mounts.json`，同步元数据保存在 `~/.config/cloudsync/metadata.db`，daemon 启动时会自动恢复所有挂载并触发一次**双向**初始扫描，将本地与 COS 的差异补齐。

**Q：暂停期间产生的变更会自动同步吗**

不会。暂停只是停止监听新事件，不会记录暂停期间的变更。恢复后建议手动运行 `cloudsync sync <path>` 触发一次全量比对。

**Q：能同时挂载多少个目录**

没有硬性限制，但所有挂载共享同一个速率限制器（默认 3 并发 / 10 QPS）。挂载数量过多时上传/下载速度会被均摊。如需调整，编辑 `~/.config/cloudsync/config.json` 中的 `performance` 部分后重启 daemon。

**Q：大文件上传会超时吗**

不会。≥ 32 MB 的文件自动切换为并发分片上传，每个分片独立重试，完整文件超时取决于网络带宽而非单次请求限制。


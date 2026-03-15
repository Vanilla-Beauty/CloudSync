# Getting Started with CloudSync

CloudSync 是一个将本地目录实时同步到腾讯云 COS 的命令行工具，采用守护进程架构，支持 `.syncignore` 过滤规则和智能防抖批量同步。

---

## 目录

- [Getting Started with CloudSync](#getting-started-with-cloudsync)
  - [目录](#目录)
  - [前置条件](#前置条件)
  - [编译安装](#编译安装)
  - [第一步：初始化配置](#第一步初始化配置)
  - [第二步：启动守护进程](#第二步启动守护进程)
  - [第三步：挂载同步目录](#第三步挂载同步目录)
  - [日常使用](#日常使用)
    - [查看状态](#查看状态)
    - [挂载多个目录](#挂载多个目录)
    - [触发同步的时机](#触发同步的时机)
    - [测试同步是否正常](#测试同步是否正常)
  - [过滤规则（.syncignore）](#过滤规则syncignore)
  - [停止与卸载](#停止与卸载)
    - [停止同步（保留远程文件）](#停止同步保留远程文件)
    - [删除远程文件并停止同步](#删除远程文件并停止同步)
    - [完整重置](#完整重置)
  - [命令速查](#命令速查)
  - [常见问题](#常见问题)

---

## 前置条件

| 项目 | 要求 |
|------|------|
| Go | 1.21 或更高版本 |
| 操作系统 | Linux / macOS / Windows |
| 腾讯云账号 | 已开通 COS 服务，准备好 SecretId、SecretKey 和 Bucket 名称 |

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

运行 `init` 命令，按提示输入 COS 凭据：

```bash
./cloudsync init
```

```
COS SecretId: your-secret-id
COS SecretKey: your-secret-key
COS Bucket: your-bucket
COS Region [ap-guangzhou]: ap-guangzhou
Config written to /home/toni/.config/cloudsync/config.json
```

也可以通过 flag 一次性传入，适合脚本环境：

```bash
./cloudsync init \
  --secret-id  "AKIDxxx" \
  --secret-key "xxx" \
  --bucket     "my-bucket-1234567890" \
  --region     "ap-guangzhou"
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

## 第三步：挂载同步目录

将本地目录挂载到 COS，开始同步：

```bash
./cloudsync mount ~/documents --prefix documents/
```

```
Mounted: /home/toni/documents → documents/ (id: a3f9c1b2)
```

挂载后 CloudSync 会立即对目录做一次**全量扫描**，将已有文件同步到 COS；之后监听文件变更，自动增量同步。

`--prefix` 是可选的，省略时默认使用目录名加 `/`：

```bash
# 等价于 --prefix projects/
./cloudsync mount ~/projects
```

验证挂载已生效：

```bash
./cloudsync ls
```

```
ID          LOCAL PATH                                REMOTE PREFIX         ADDED AT
------------------------------------------------------------------------------------------
a3f9c1b2    /home/toni/documents                      documents/            2026-03-15T10:00:00+08:00
```

---

## 日常使用

### 查看状态

```bash
./cloudsync status    # 守护进程信息 + 所有挂载列表
./cloudsync ls        # 仅列出挂载列表
```

### 挂载多个目录

```bash
./cloudsync mount ~/projects  --prefix dev/projects/
./cloudsync mount ~/pictures  --prefix media/pictures/
./cloudsync mount ~/notes     --prefix notes/
```

### 触发同步的时机

CloudSync 使用**防抖 + 批量**策略，不是每次保存立刻上传：

1. 文件变更后开始 **2 秒**倒计时
2. 倒计时期间再次变更则重置计时器（防抖）
3. 倒计时结束，文件进入批量队列
4. 批量队列每 **5 秒**或累计 **100 个文件**时统一上传

因此保存文件后约 **2～7 秒**内完成同步，这是设计预期行为。

### 测试同步是否正常

```bash
mkdir /tmp/synctest
./cloudsync mount /tmp/synctest --prefix test/

echo "hello cloudsync" > /tmp/synctest/hello.txt
# 等待约 3 秒...
# 登录腾讯云 COS 控制台确认 test/hello.txt 已上传
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

# Office 编辑锁文件（自动检测，这里仅作示例）
~$*

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

- 前缀：`~$`（Office 锁）、`.#`（Emacs 锁）、`#`（Emacs 自动保存）
- 后缀：`~`、`.tmp`、`.swp`、`.swo`、`.temp`、`.bak`
- 扩展名：`.tmp`、`.swp`、`.swo`、`.temp`、`.bak`、`.cache`

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
```

---

## 命令速查

```
cloudsync init [--secret-id STR] [--secret-key STR] [--bucket STR] [--region STR]
    配置 COS 凭据，写入 config.json

cloudsync start
    启动守护进程（已运行则提示 already running）

cloudsync stop
    停止守护进程

cloudsync status
    显示守护进程状态和所有挂载目录

cloudsync mount <path> [--prefix <remote-prefix>]
    开始同步指定目录

cloudsync unmount <path>
    停止同步，远程文件保留

cloudsync delete <path>
    停止同步并删除所有远程文件（需交互确认）

cloudsync ls
    列出所有当前活动的挂载
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

守护进程未启动，执行 `cloudsync start`。

**Q：文件已保存但 COS 上没有出现**

1. 检查 `.syncignore` 规则是否误过滤了该文件
2. 检查文件名是否触发了交换区检测（以 `~$`、`.#` 开头，或以 `.tmp`、`.swp` 等结尾）
3. 查看 daemon 日志确认有无错误：`tail -f ~/.config/cloudsync/cloudsyncd.log`
4. 确认 COS 凭据和 Bucket 名称正确：`cat ~/.config/cloudsync/config.json`

**Q：`cloudsync init` 后凭据如何更新**

直接重新运行 `cloudsync init`，会覆盖旧配置。修改后需要重启 daemon：
```bash
cloudsync stop && cloudsync start
```

**Q：daemon 重启后之前挂载的目录还在吗**

是的。挂载列表保存在 `~/.config/cloudsync/mounts.json`，daemon 启动时会自动恢复所有挂载并触发一次全量扫描。

**Q：能同时挂载多少个目录**

没有硬性限制，但所有挂载共享同一个速率限制器（默认 3 并发 / 10 QPS）。挂载数量过多时上传速度会被均摊。如需调整，编辑 `~/.config/cloudsync/config.json` 中的 `performance` 部分后重启 daemon。

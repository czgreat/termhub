# Linux 节点

2026-10-11 起 termhub 的节点程序（agent）也能跑在 Linux 上，已在 Ubuntu 26.04（x86-64，systemd）上测试。思路和 Windows 节点完全一样：节点主动连 Hub，网页里开终端和 AI CLI；只是终端从 ConPTY 换成了 POSIX 伪终端。

本文讲 Linux 节点和 Windows 节点不一样的地方，以及怎么装。Linux 节点目前没有发布包，要从源码构建。

## 和 Windows 节点的对应关系

| 部分 | Windows | Linux |
|---|---|---|
| 终端 | ConPTY（`internal/session/conpty_windows.go`） | `/dev/ptmx` 伪终端（`pty_linux.go`），根进程用 `setsid` 开新的终端会话并以伪终端为控制终端 |
| 启动 CLI | `pwsh -NoExit -Command & 'claude' ...` | `bash -l -i -c 'claude ...; exec bash -l'`：登录且交互的 bash，`~/.profile`、`~/.bashrc` 里加的 PATH 都生效，CLI 退出后回到提示符 |
| 结束会话 | 关伪控制台 → Job Object → 结束追踪到的后代 | SIGHUP 给会话首进程组和前台进程组 → SIGKILL 整个终端会话 → 用 pidfd 结束追踪到的后代 |
| 进程身份 | pid + 创建时间（句柄） | pid + 启动时刻（`/proc/<pid>/stat` 第 22 项加开机时刻），杀进程前用 pidfd 核对，pid 被复用也不会误杀 |
| host 与 agent 之间 | 命名管道，只允许本用户（DACL） | Unix 套接字 `$XDG_RUNTIME_DIR/termhub/host.sock`：目录 0700、套接字 0600、核对对端 uid；旁边的 `host.sock.lock` 用排他锁保证只有一个 host |
| 节点令牌 | DPAPI 加密 | AES-256-GCM，密钥由 `/etc/machine-id` 和 uid 派生；配合目录 0700、`agent.json` 0600 且属本用户，否则拒绝读取（说明见下） |
| 机器指纹 | 注册表 MachineGuid 的哈希 | `/etc/machine-id` 的哈希 |
| 自启动 | 计划任务 | systemd 用户服务 `termhub-agent.service`，`KillMode=process`；开启 linger 后不需要登录 |
| 安装目录 | `%LOCALAPPDATA%\termhub` | `~/.local/share/termhub`（`$XDG_DATA_HOME/termhub`） |
| 目录选择器 | 盘符、桌面 | 一个根 `/`、“主目录”按钮（`/home/<用户>`） |
| 粘贴图片 | 放进剪贴板，再发 Alt+V | 没有图形剪贴板：图片上传到节点，路径粘进终端，可以让 Claude Code 读这个路径 |

### Hub 和网页

- 路径以 `/` 开头（但不是 `//`）的就是 Linux 节点的文件夹。规则见 `internal/hub/route/registry.go` 的 `posixPath`、`normFolder`。
- Linux 文件夹**区分大小写**，分隔符是 `/`；`.`、`..` 和多余的 `/` 会被规整。Windows 文件夹的规则不变。
- 网页的 `folderKey`（`web/src/lib/history.svelte.ts`）必须和 Hub 的 `normFolder` 给出同一个键，两边各有一份同样的用例（`posix_test.go`、`history.test.mjs`）。
- 文件夹范围（绑定里“只显示这些文件夹”）可以写 `/home/alice/work` 这样的路径。
- CLI 配置模板多了“Claude Code（Linux）”“Codex（Linux）”“Bash（Linux）”，外壳是 `/bin/bash`。

### 令牌加密的边界

Linux 没有 DPAPI。令牌用由本机 machine-id 和 uid 派生的密钥加密，所以 `agent.json` 拷到别的机器（或备份里）是打不开的。但本机的同一用户和 root 能算出同样的密钥，所以对本机其他用户真正起保护作用的是文件权限：目录 0700、文件 0600、属主是本用户，这三条不满足 agent 就拒绝启动，和 ssh 对私钥的要求一样。

### 已知限制

- 一个后代进程如果既离开了终端会话（`setsid`、守护进程化），又在追踪器两次扫描之间（2 秒）就成了孤儿，它找不到，结束会话时也杀不到。Windows 上逃出 Job 的进程是同样的情况。
- 没有剪贴板，粘贴图片只能给路径。

## 安装（以要使用的那个用户身份）

1. 在 Hub 的“管理 → 节点”里添加节点，记下令牌和证书指纹。
2. 在构建机上编译 agent（agent 不内嵌网页，不用先构建网页）：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o termhub-agent ./cmd/termhub-agent`。把它和 `deploy/linux/` 里的三个脚本拷到节点上同一个文件夹。
3. 运行（令牌在提示时粘贴，不进命令行和历史）：

   ```bash
   bash install-agent.sh --hub https://192.168.1.10:27443 --pin <指纹>
   ```

   脚本把程序装到 `~/.local/share/termhub/`，登记（先真连一次 Hub，失败就什么都不写），写 systemd 用户服务并启动，再开启 linger（`loginctl enable-linger`，可能要 sudo）。
4. 在“管理 → CLI 配置”里用 Linux 模板给这个节点建配置，默认目录和绑定按需要填。

升级：`bash upgrade-agent.sh --exe ./termhub-agent [--sha <sha256>]`，只重启 `run`，host 和会话不断，旧程序留作 `termhub-agent.old-<版本>`。卸载：`bash uninstall-agent.sh`，会结束这台机器上的所有 termhub 会话。

日志：`~/.local/share/termhub/agent/logs/agent.log`、`~/.local/share/termhub/host/logs/host.log`；服务状态：`systemctl --user status termhub-agent`。

## 开发

- Linux 专属代码都在 `*_linux.go`；两边共用的逻辑在不带后缀的文件里。改共用代码时两个平台都要编译：`GOOS=windows go vet ./... && GOOS=linux go vet ./...`。
- Linux 测试要在 Linux 上跑，只用假 CLI `tools/testcli`，**不在测试里启动真实的 Claude Code / Codex，不拿真实账号发请求**。
- `test/e2e` 的公共部分（`e2e_test.go`、`helpers_test.go`）两个平台共用；`linux_test.go` 是 Linux 节点的全链路用例。

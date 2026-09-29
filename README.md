# termhub

在浏览器和手机上使用局域网里多台 Windows 电脑上的终端，以及跑在终端里的 AI 编程 CLI（Claude Code、Codex 等）。官方 CLI 一行不改，termhub 只在终端这一层工作。

*Use the terminals — and the AI coding CLIs running in them (Claude Code, Codex, …) — of several Windows PCs on your LAN from a browser or a phone. The official CLIs are not modified; termhub works at the terminal level. English summary at the end.*

## 能做什么

- **会话不怕断**：会话跑在那台 Windows 上。关掉浏览器、断网、换设备都不影响，重新打开接着看、接着用。
- **多台机器、多个会话**：侧栏按机器分组，支持标签和分屏。状态点显示哪个会话在干活、哪个做完了、哪个在等你选择。
- **手机友好**：可以装成 PWA。底部有输入框，程序给出的选项会变成可以点的卡片，往上翻看后一点就回到底部。
- **历史对话**：按项目文件夹列出 Claude Code 和 Codex 的历史对话，可以只读查看，也可以接着继续（只读取官方历史文件，从不修改）。
- **文件**：粘贴或拖入图片和文件会上传到那台电脑；点终端里的路径就能下载，文件夹打包成 zip。
- **花费与上下文**：每个 AI 会话按官方价格折算美元，并显示上下文占用。管理员可以改价格表。
- **账号安全**：密码加 TOTP 两步验证（强制），支持受信任设备和恢复码，登录有限速，重要操作有审计记录。节点只认 Hub 证书的指纹。

## 架构

```
浏览器 / 手机 PWA
    │  HTTPS + WebSocket
    ▼
Hub（Go 单程序，内嵌网页和 SQLite；Linux 上用 Docker 跑，也可以直接运行）
    ▲  节点主动连 Hub（HTTPS，只认 Hub 证书指纹）
    │
每台 Windows 上的 termhub-agent
    └─ 会话宿主 ─ ConPTY ─ pwsh / Claude Code / Codex …
```

- Hub 负责账号、网页、节点登记和会话转发，不保存终端内容。
- agent 以登记时那个 Windows 用户的身份运行。CLI 的登录状态、PATH、配置目录都和那个用户平时用的一样。
- 会话宿主和 agent 是两个进程，升级或重启 agent 时正在跑的会话不会断。

## 界面预览

以下为真实网页界面，使用虚构的机器、项目和对话数据；没有连接真实节点或 AI 服务。

![桌面终端与机器列表](docs/images/desktop.png)

<img src="docs/images/mobile.png" alt="手机上的历史对话" width="390">

## 第一个版本

v0.1.0 是第一个对外发行版本。下载 [Release](https://github.com/czgreat/termhub/releases) 中的预编译包，按 [发布包安装说明](docs/发布包安装.md) 操作，无需安装 Go 或 Node.js。源码部署见下文。

- Linux Hub：amd64、arm64；ARM64 包交叉编译，尚未做 ARM 真机验收。
- Windows agent：amd64，附安装、升级、卸载脚本。
- 默认在 Windows 用户登录后启动；需要无人登录也在线，请选择安装脚本的 `-AtStartup`。两者不是同一种启动方式。
- 包未做代码签名，下载后请核对同一 Release 的 SHA256SUMS.txt。

## 部署

### 方式一：让 AI 代理帮你部署

把下面这段复制给 Claude Code、Codex 或其他能执行命令的 AI 代理，它会先读仓库里的部署说明，问清你的环境，然后一步步做。需要输入密码、确认或在浏览器里操作的地方，它会停下来交给你。

```text
请阅读 https://github.com/czgreat/termhub/blob/main/docs/AGENT-DEPLOY.md ，严格按照其中的步骤和安全规则帮我部署 termhub。先问清楚我的环境，再一步步执行；需要密码、确认或在浏览器里操作的地方停下来交给我。
```

### 方式二：自己动手

见 [docs/部署指南.md](docs/部署指南.md)。大致步骤：

1. 准备一台装有 Docker 的 Linux 主机（NAS、小主机都行），在构建机上装好 Go 1.27+ 和 Node.js 22.13+。
2. 运行 `bash deploy/release.sh user@主机`。第一次运行会在目标主机的 `~/termhub` 里生成 `.env` 和 `hub.env`，填好地址后再运行一次：它会构建网页、Hub 和 Windows agent，再在目标主机上构建镜像并启动。
3. 浏览器打开 `https://主机:27443`，用日志里的一次性令牌创建第一个管理员，绑定 TOTP。
4. 在“管理 → 节点”里添加节点，把 `termhub-agent.exe` 和 `deploy/install-agent.ps1`、`deploy/upgrade-agent.ps1`、`deploy/uninstall-agent.ps1` 拷到那台 Windows 上运行。
5. 在“管理 → CLI 配置”里用模板给节点加上 PowerShell、Claude Code、Codex，然后绑定给用户。

## 现状与限制

- **节点目前只支持 Windows**：agent 基于 Windows 的 ConPTY、计划任务和 DPAPI 实现。Linux（Ubuntu、Debian 等）和 macOS 节点还没有做。Hub 本身在 Linux 上运行，不受影响。
  - 协议（`internal/proto`）和 Hub 与平台无关，加 Linux 节点主要是写一个基于 pty 的会话宿主和 agent。
  - 欢迎提交合并请求。
- 网页和大部分文档是中文。
- 本版通过脚本安装和升级；不含自动升级、托盘程序或出口 IP 监控。
- 手机截图来自浏览器模拟；iPhone、Android 真机的完整验收尚未完成。
- 花费显示按官方公开价格估算，不是账单。

## 安全提示

- 27443 端口只应在局域网内可达。要从外网访问，请放在带 HTTPS 的反代或隧道后面（见 [安全说明](docs/安全说明.md)），不要把 27443 直接暴露到公网。
- 把某个 CLI 配置绑定给某个用户，就等于允许他通过网页访问那台机器上那个 Windows 用户能访问的全部文件。**绑定不是安全隔离**，只绑给你信任的人。
- `data/` 目录里有数据库、主密钥和证书，请整个备份。丢了 `master.key`，TOTP 密钥和配置里的密文就无法解开。

## 开发

- 需要 Go 1.27+、Node.js 22.13+。Windows 专属的包只能在 Windows 上编译和测试。
- 网页：`cd web && npm ci && npm run build`（产物嵌入 Hub，编译 Hub 前必须先构建网页）、`npm run check`、`npm run test:unit`。
- Go：`go vet ./...`、`go test ./...`。`test/e2e` 会起本地 Hub、假节点和 Playwright 浏览器，全程用假 CLI（`tools/testcli`），不需要任何账号或密钥。
- 实现结构见 [架构说明](docs/架构.md)。给 AI 代理的开发须知在 [AGENTS.md](AGENTS.md)。

## 许可

[MIT](LICENSE)。借用的上游代码和随程序分发的依赖见 [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES)。

---

## English summary

termhub lets you reach the terminals of several Windows PCs on your LAN from a browser or phone, with first-class support for AI coding CLIs (Claude Code, Codex). A Go **Hub** (single binary with the web UI and SQLite embedded, usually run in Docker on a Linux box) talks to a **termhub-agent** on each Windows PC, which hosts ConPTY sessions that survive browser disconnects and agent restarts. Features: tabs and split panes, session status dots, a mobile PWA with tappable option cards, read-only browsing and resuming of CLI history, file upload and download, per-session cost and context estimates, and mandatory TOTP.

- Let an AI agent deploy it: paste the prompt above (it points the agent at [docs/AGENT-DEPLOY.md](docs/AGENT-DEPLOY.md)).
- Manual deployment: [docs/部署指南.md](docs/部署指南.md) (Chinese; commands are self-explanatory).
- Nodes are Windows-only for now (ConPTY, Task Scheduler, DPAPI); Linux/macOS nodes are not implemented — pull requests welcome. The Hub runs on Linux.
- The UI and most docs are in Chinese.
- License: MIT.

## 友情链接

- [LINUX DO 社区](https://linux.do/)

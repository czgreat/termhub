# 给 AI 代理的 termhub 部署说明

> 读者：受用户委托、帮他部署 termhub 的 AI 编程代理（Claude Code、Codex 等）。
> 用户把 README 里的一段话发给你，你就读到了这里。请从头到尾按顺序执行。
> 本文只讲部署。要改代码，请读仓库根目录的 `AGENTS.md`。

## 你的任务

帮用户把 termhub 部署起来：

- 一台 Linux Docker 主机上跑 Hub；
- 一台或几台 Windows 电脑装上 agent；
- 最后用户能在浏览器里打开 Windows 上的终端和 AI CLI。

人类的完整步骤在 [docs/部署指南.md](部署指南.md)。本文在它的基础上规定你该怎么做、哪些事必须交给用户。

## 安全规则（必须遵守）

1. **不要索要密码和密钥，也不要让用户把它们贴进对话。** 以下几样都由用户本人在浏览器、弹窗或自己的终端里输入：
   - Windows 密码（`-AtStartup` 时由 Windows 的凭据弹窗收取）；
   - termhub 管理员密码；
   - TOTP 动态码和恢复码；
   - CLI 的 API 密钥或登录。
2. `setup_token` 只转告用户，由他在浏览器里用。节点令牌 `thn_…` 长期有效，等同于这台节点的密码：最好让操作那台电脑的人在 `install-agent.ps1` 的提示里自己粘贴；你必须经手时，不要写进任何文件或提交，用完提醒用户可以在“管理 → 节点 → 换令牌”里轮换。
3. 不要把 27443 端口发布到公网，也不要替用户改路由器、防火墙或 DNS。用户要外网访问时，只按部署指南第 6 节给出建议，由他决定。
4. 不修改官方 CLI（Claude Code、Codex），也不动它们的登录方式、配置和历史文件。
5. 不要改用户机器上与 termhub 无关的东西。遇到已有的 `~/termhub` 目录、已存在的计划任务 `termhub-agent`、端口被占用，先停下来问用户。
6. 需要管理员权限、会重启服务、会删除数据的操作，先说明要做什么，得到同意再做。
7. 如实汇报：哪一步做了、结果如何、哪一步没做或失败了。

## 第 1 步：问清环境

开始前一次问清，不要边做边猜：

1. Docker 主机：地址、ssh 用户（能否免密登录）、系统和 CPU 架构（amd64 或 arm64）、是否已装 Docker 和 compose 插件。
2. 你现在运行在哪台机器上（它就是构建机）。它能不能 ssh 到 Docker 主机，还是说你本身就在 Docker 主机上。
3. 要接入的 Windows 电脑：几台；每台用哪个 Windows 用户；你能否在那台电脑上执行命令（能的话怎么执行）。不能的话，就由用户自己运行你给出的命令。
4. 浏览器会用哪些地址访问 Hub。通常是 `https://<Docker 主机局域网地址>:27443`。
5. 每台电脑上要用哪些 CLI（PowerShell、Claude Code、Codex），是否已经装好并登录。

## 第 2 步：检查构建机

```bash
git --version && go version && node --version && npm --version
```

- 要求 Go 1.27 或更高，Node.js 22.13.0 或更高（建议使用 CI 同款 22.x）。
- 缺了就告诉用户，征得同意后再装。Go 模块下载慢时可以设 `GOPROXY`。
- Windows 构建机上用 Git Bash 运行 `.sh` 脚本。

## 第 3 步：取代码并跑一遍快速检查

```bash
git clone https://github.com/czgreat/termhub.git termhub && cd termhub
cd web && npm ci && npm run build && npm run test:unit && cd ..
go vet ./cmd/termhub ./internal/hub/... ./internal/proto
```

- 这一步只在本机运行，不连任何节点。
- 不要跑 `go test ./...`：端到端测试要在 Windows 上起浏览器，耗时较长，部署用不到。

## 第 4 步：部署 Hub

按部署指南第 2 节执行 `bash deploy/release.sh user@host`（或在 Docker 主机本机执行 `bash deploy/release.sh local`）。

1. 第一次运行会生成 `.env` 和 `hub.env` 后停下。这是正常的。
2. 按第 1 步得到的信息填好这两个文件：
   - `TH_BIND_ADDR`：Docker 主机的局域网地址；
   - `TH_PUBLIC_URL`：浏览器访问的地址。
3. 再运行一次。
4. 成功的标志是输出 `health: healthy`。
5. 从输出里找到 `setup_token` 和 `cert_fingerprint`：
   - 指纹可以告诉用户，并记住它，第 6 步要用。
   - `setup_token` 只告诉用户，让他自己在浏览器里用。

失败时：

- 看 `docker logs termhub` 的最后几十行。
- 最常见的原因是 `TH_PUBLIC_URL` 没填或格式不对，以及端口被占用。

## 第 5 步：用户创建管理员（交给用户）

请用户：

1. 在浏览器打开 `TH_PUBLIC_URL`，接受自签证书的提示。
2. 用 `setup_token` 创建管理员。
3. 用手机验证器绑定 TOTP，并保存恢复码。

等用户确认完成再继续。

## 第 6 步：接入 Windows 电脑（每台一次）

1. 请用户在网页“⚙ 管理 → 节点”里添加节点，把显示出来的登记命令交给**那台电脑上的操作者**。这个操作者可以是你（如果你能在那台电脑上执行命令），也可以是用户本人。令牌只显示一次，也只用于这一台。
2. 把 `dist/termhub-agent.exe` 和 `deploy/install-agent.ps1`、`deploy/upgrade-agent.ps1`、`deploy/uninstall-agent.ps1` 放到那台电脑的同一个文件夹里。
3. 以要使用的那个 Windows 用户身份，在普通（不提权）的 PowerShell 窗口里运行：
   ```powershell
   Set-ExecutionPolicy -Scope Process Bypass
   .\install-agent.ps1 -Hub <TH_PUBLIC_URL 里的局域网地址> -Pin <cert_fingerprint>
   ```
   脚本会提示粘贴节点令牌。
4. 要不要开机自启（不需要有人登录）由用户决定。要的话加 `-AtStartup`，安装账号本身须属于管理员组，并在同一用户“以管理员身份运行”的 PowerShell 里执行；不要换成另一个管理员身份；Windows 会让用户本人输入密码，你不要代填。那台电脑上已有 termhub 任务（另一个 Windows 用户装过）时，加 `-TaskName termhub-agent-<用户名>`。
5. 成功的标志是网页“管理 → 节点”里这台电脑显示“在线”。

失败时看 `%LOCALAPPDATA%\termhub\agent\logs\agent.log`，常见原因有三个：

- 地址不通：防火墙挡住了 27443；
- 指纹不符；
- 令牌不对，或已在“换令牌”后作废。

## 第 7 步：CLI 配置与用户（交给用户，你可以指导）

请用户在“管理 → CLI 配置”里点模板（PowerShell、Claude Code、Codex），为每个节点添加配置，并在“绑定给用户”里勾选自己。

要提醒用户：绑定等于允许那个人访问这台电脑上那个 Windows 用户的全部文件，这不是安全隔离。

## 第 8 步：验收

请用户在网页上做这几件事：

1. 新建一个 PowerShell 会话，输入 `whoami`，确认输出是预期的 Windows 用户。
2. 关掉浏览器标签再打开，会话还在，画面能接上。
3. 如果配置了 Claude Code 或 Codex，新建一个会话，确认 CLI 能正常启动。这一步会消耗用户自己的额度，先问一下用户。

最后给用户一份简短汇报，包含：

- Hub 地址和版本（网页左下角）；
- 接入了哪些节点，各自用哪个 Windows 用户，是否开机自启；
- 做了什么、没做什么；
- 数据备份提醒：Docker 主机上的 `~/termhub/data` 要整个备份；
- 升级方法：见部署指南第 7 节。

## 参考

- 部署细节与常见问题：[docs/部署指南.md](部署指南.md)
- Hub 配置项、端口、反代要求：[安全说明](安全说明.md)
- agent、计划任务、登记：[部署指南](部署指南.md)
- 账号与认证：[安全说明](安全说明.md)

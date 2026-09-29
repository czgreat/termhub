# termhub 开发须知（给 Codex、Claude Code 等代理）

termhub 让你在浏览器（含手机）里使用局域网 Windows 电脑上的终端和 AI CLI（Claude Code、Codex）。它由三部分组成：

- Go 写的 Hub，通常跑在 Linux 的 Docker 里；
- 每台 Windows 电脑上的 agent；
- Svelte 5 写的网页。

先读 `README.md`、`docs/架构.md` 和 `docs/安全说明.md`。

- 要**部署**：读 `docs/AGENT-DEPLOY.md`。
- 要**改代码**：读本文。

## 硬规则

1. **不改官方 CLI**：不修改 Claude Code、Codex 本身，也不动它们的登录方式和凭据配置。termhub 只在终端层面工作。
2. **官方历史文件只读**：termhub 只读取 CLI 的历史文件，从不写、不删、不移动。
3. **测试不用真实 CLI 和账号**：自动化测试一律用假 CLI（`tools/testcli`），不启动真实的 Claude Code、Codex，不需要任何 API 密钥。需要和真实 CLI 核对时，只在专门的测试机上做，由维护者手动进行。
4. **不在别人正在用的机器上加压**：不做压测，不反复跑浏览器测试。
5. **不提交敏感信息**：真实地址、账号、令牌、密钥、证书、个人路径都不进仓库。示例一律用 `192.168.1.10`、`example.com`、`C:\Users\me` 这类占位值。
6. **安全相关改动要复核**：涉及认证、会话路由、文件访问、agent 权限的改动，要由另一个人（或另一个代理、模型）复核后再合并。

## 开发与测试

- 需要 Go 1.27 或更高、Node.js 22.13.0 或更高（建议使用 CI 同款 22.x）。Windows 专属的包（agent、会话宿主、ConPTY）只能在 Windows 上编译和测试。
- **网页**（在 `web/` 下执行）：
  - `npm ci`、`npm run build`：产物进 `internal/hub/web/dist/`，编译 Hub 前必须先构建网页；
  - `npm run check`：svelte-check；
  - `npm run test:unit`：node:test，不开浏览器、不连节点。
- **Go**：`go vet ./...`、`go test ./...`。
  - `test/e2e` 会起本地 Hub、假节点和 Playwright 浏览器，一次约 70 秒。每轮改动跑一次就够，不要反复跑。
- 新增的行为要有测试。最好先确认测试在旧代码上是失败的。
- 注释和文档风格跟随周围的代码。注释说明“为什么”，必要时注明出处（例如相关函数或公开问题单）。

## 结构

```
cmd/termhub/         Hub
cmd/termhub-agent/   每台 Windows 上的程序：enroll、run、host
internal/            各模块的实现，包名与职责见 docs/架构.md
web/                 网页（Svelte 5 + xterm.js）
tools/testcli/       测试用的假 CLI
tools/s0check/       运行环境自检
test/e2e/            端到端测试
deploy/              Docker、发布脚本、Windows 安装脚本
docs/                设计文档与部署文档
```

## 发布

见 `docs/部署指南.md`：

- Hub 用 `bash deploy/release.sh user@docker-host` 或 `bash deploy/release.sh local`，版本号是 `0.1.0-<短哈希>`；
- agent 用 `deploy/install-agent.ps1` 安装，用 `deploy/upgrade-agent.ps1` 升级。
- 有数据库迁移时，Hub 启动前会自动把数据库备份到 `data/backups/pre-migrate-vN-*.db`。迁移后旧版 Hub 打不开新库，回退时要把备份一起换回去。

# termhub

[中文](README.md) | English

termhub makes terminals on Windows PCs available in a browser. Use it to manage sessions across several machines, read output on your phone, run commands, or continue a Claude Code or Codex conversation.

Terminal processes run on the Windows node. Closing the browser or losing its connection leaves sessions running under the node's session host. You can reconnect later. Shutting down the node or exiting the terminal process ends the session.

## Features

- Sessions grouped by machine, with tabs and split panes.
- Claude Code and Codex conversation history, organized by project folder, with support for resuming sessions. Official history files are read-only.
- Image and file uploads, file downloads, and folder downloads as ZIP archives.
- A mobile browser interface and PWA, with an input box and tappable terminal choices.
- Session status, context usage, and estimated costs. Estimates are not billing records.
- Password and TOTP authentication, recovery codes, trusted devices, and audit logging.

termhub does not modify the official CLIs or replace their installation, login, or subscription requirements.

## Screenshots

These screenshots use fictional machines, projects, and conversations. The mobile view was captured using browser emulation.

![Desktop terminal and machine list](docs/images/desktop.png)

<img src="docs/images/mobile.png" alt="Conversation history on a phone" width="390">

## Installation

You need a Linux host for the Hub and at least one Windows PC as a node. The Hub installation guides use Docker and Compose; Windows nodes use PowerShell 7.

v0.1.0 is the first release. Once published, packages are available from [Releases](https://github.com/czgreat/termhub/releases):

| Component | Platform |
| --- | --- |
| Hub | Linux amd64 / arm64 |
| agent | Windows amd64 |

- **Prebuilt packages**: follow the [package installation guide](docs/发布包安装.md) (Chinese). Go and Node.js are not required.
- **Build from source**: follow the [deployment guide](docs/部署指南.md) (Chinese). Requires Go 1.27+ and Node.js 22.13+.

The ARM64 package has not been tested on ARM hardware. Packages are unsigned; verify their source and the SHA256SUMS.txt from the same release.

### Deploy with an agent

If your agent can read repositories and run commands, give it the prompt below. You will still need to create accounts, enter credentials, and sign in to the official CLIs yourself. The linked deployment instructions are in Chinese.

```text
Help me deploy termhub. First read https://github.com/czgreat/termhub/blob/main/docs/AGENT-DEPLOY.md and its linked installation guides. If you cannot access them, tell me and wait for me to provide the files; do not guess the steps. Confirm the target hosts, Windows user, access URL, version, and whether this is a new installation or an upgrade. Prefer packages for that version; explain the source-build option if no suitable package is available. Follow the documentation for the selected version. Before modifying an existing installation, explain the impact and obtain my confirmation. Do not ask me to paste passwords or tokens into chat, and do not alter official CLI configuration or login settings. Pause when I need to enter credentials. Finish by reporting versions, node status, completed checks, and anything left unfinished.
```

## How it runs and current limitations

Browsers connect to the Hub over HTTPS / WebSocket. Windows agents initiate connections to the Hub. The Hub serves the web interface, manages accounts, and forwards sessions; a session host on each node owns the terminal processes. See the [architecture guide](docs/架构.md) (Chinese).

- Nodes currently require Windows. Linux and macOS nodes are not supported. The web interface is primarily in Chinese.
- By default, the agent starts three minutes after Windows user logon. For startup without logon, follow the installation guide for `-AtStartup`, using the same installation account with administrator privileges.
- Updating the agent connection process does not update an existing session host. Updating the host requires a separately planned session shutdown and restart.
- Installation and upgrades use scripts. There is no automatic updater or tray application.
- Full testing on physical iPhone and Android devices is not yet complete.

## Access and backups

Binding a node's CLI configuration to a user gives that user access to files available to the Windows account running the node. This is not filesystem isolation. Grant access only to people you trust.

Keep Hub port 27443 restricted to your LAN. For remote access, configure an HTTPS reverse proxy or tunnel as described in the [security guide](docs/安全说明.md) (Chinese). Back up the entire `data/` directory, including the database, master key, and certificates.

## Development

The frontend is in `web/`, the Hub in `internal/hub/`, and the agent in `internal/agent/`. Build the frontend before compiling the Hub:

```sh
cd web
npm ci
npm run build
npm run check
npm run test:unit
cd ..
go test ./...
```

The full Go test suite requires Windows and includes browser end-to-end tests using a fake CLI. See [AGENTS.md](AGENTS.md) for development instructions.

## License

[MIT](LICENSE). Third-party license notices are in [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).

## Community

- [LINUX DO](https://linux.do/)

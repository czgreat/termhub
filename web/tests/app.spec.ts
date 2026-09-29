// The whole thing through a real browser: first login with TOTP enrolment,
// adding a node and enrolling a real agent, a CLI profile, a new project
// folder, a terminal session, typing, a page reload (replay), ending it.
// The CLI is the project's test CLI, never a real one (docs/M12 第 4 节).
import { test, expect, devices, type Page } from '@playwright/test'
import { spawn, execSync, type ChildProcess } from 'node:child_process'
import { createHmac } from 'node:crypto'
import { mkdtempSync, mkdirSync, existsSync, writeFileSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { sessionInput } from './input-wire'

const env = (k: string) => {
  const v = process.env[k]
  if (!v) throw new Error(`missing ${k}: run this through test/e2e (go test ./test/e2e -run Browser)`)
  return v
}

function totp(secretB32: string): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of secretB32.replace(/=+$/, '')) bits += alphabet.indexOf(c).toString(2).padStart(5, '0')
  const key = Buffer.from(bits.match(/.{8}/g)!.map(b => parseInt(b, 2)))
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 1000 / 30)))
  const h = createHmac('sha1', key).update(msg).digest()
  const off = h[h.length - 1] & 0x0f
  const v = ((h[off] & 0x7f) << 24 | h[off + 1] << 16 | h[off + 2] << 8 | h[off + 3]) % 1_000_000
  return String(v).padStart(6, '0')
}

const password = 'correct horse battery staple'
let codes: string[] = [] // recovery codes from the first test; the phone test logs in with one
let projectRoot = ''
let histCwd = '' // the folder of the fake conversations (history test), for the phone test
test.describe.configure({ mode: 'serial' })

/** Text of the visible terminal's buffer in pane `pane`; the WebGL renderer draws on a canvas, so the DOM has none. */
async function termText(page: Page, pane = 0): Promise<string> {
  return page.evaluate((pane) => {
    const p = document.querySelectorAll('[data-testid=pane]')[pane]
    const host = Array.from(p?.querySelectorAll('.xterm-host') ?? []).find(e => (e as HTMLElement).offsetParent !== null) as any
    const t = host?.__term
    if (!t) return ''
    const b = t.buffer.active
    const lines: string[] = []
    for (let i = 0; i < b.length; i++) lines.push(b.getLine(i)?.translateToString(true) ?? '')
    return lines.join(String.fromCharCode(10))
  }, pane)
}
const expectTerm = (page: Page, s: string, pane = 0, timeout = 30_000) => expect.poll(() => termText(page, pane), { timeout }).toContain(s)
/** Selects the whole buffer line containing `s` in pane `pane`'s visible terminal. */
async function selectLine(page: Page, pane: number, s: string) {
  await page.evaluate(({ pane, s }) => {
    const p = document.querySelectorAll('[data-testid=pane]')[pane]
    const host = Array.from(p?.querySelectorAll('.xterm-host') ?? []).find(e => (e as HTMLElement).offsetParent !== null) as any
    const t = host.__term
    const b = t.buffer.active
    for (let i = 0; i < b.length; i++) {
      const line = b.getLine(i)?.translateToString(true) ?? ''
      if (line.includes(s)) { t.select(0, i, line.length); return }
    }
    throw new Error('line not found: ' + s)
  }, { pane, s })
}
const paneTerm = (page: Page, pane: number) => page.getByTestId('pane').nth(pane).locator('.xterm-helper-textarea')
let agent: ChildProcess | undefined

test.afterAll(() => { agent?.kill() })

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('用户名').fill('root')
  await page.getByLabel('密码').fill(password)
  await page.getByRole('button', { name: '登录' }).click()
}

test('first login, node enrolment, new project, terminal, reload, end', async ({ page }) => {
  const url = env('TH_URL')
  const pin = env('TH_PIN')
  const agentExe = env('TH_AGENT_EXE')
  const testcli = env('TH_TESTCLI')
  const localAppData = mkdtempSync(join(tmpdir(), 'termhub-web-'))
  const pipe = `\\\\.\\pipe\\termhub-browser-${Date.now()}`

  // Setup: the one-time token from the Hub's log creates the administrator.
  await page.goto('/')
  await expect(page.getByText('还没有任何用户')).toBeVisible()
  await page.getByLabel('初始化口令').fill(env('TH_SETUP_TOKEN'))
  await page.getByLabel('管理员用户名').fill('root')
  await page.getByLabel('密码（至少 11 位）').fill(password)
  await page.getByRole('button', { name: '创建管理员' }).click()

  // First login: no second factor yet, enrolment is forced.
  await page.getByLabel('用户名').fill('root')
  await page.getByLabel('密码').fill(password)
  await page.getByRole('button', { name: '登录' }).click()
  await page.getByRole('button', { name: '生成密钥' }).click()
  const secret = (await page.getByTestId('totp-secret').textContent())!.trim()
  expect(secret.length).toBeGreaterThan(16)
  await page.getByLabel('输入验证器显示的 6 位动态码以确认').fill(totp(secret))
  await page.getByRole('button', { name: '确认绑定' }).click()
  codes = (await page.getByTestId('recovery-codes').textContent())!.trim().split('\n')
  expect(codes).toHaveLength(10)
  await page.getByRole('button', { name: '我已保存，进入' }).click()
  // The quick-start guide opens by itself the first time, on the 电脑 tab.
  await expect(page.getByTestId('guide')).toBeVisible()
  await expect(page.getByTestId('guide')).toContainText('Ctrl+Shift+U')
  await page.getByTestId('guide-close').click()
  await expect(page.getByTestId('guide')).toHaveCount(0)
  await expect(page.getByTestId('new-session')).toBeVisible()
  // Mouse users get a quick hint on the icon buttons; the title comes back after.
  await page.getByTestId('split-row').hover()
  await expect(page.locator('.tip.show')).toHaveText('左右分屏（Ctrl+Shift+D）')
  await page.getByTestId('new-session').hover()
  await expect(page.locator('.tip.show')).toHaveCount(0)
  await expect(page.getByTestId('split-row')).toHaveAttribute('title', '左右分屏（Ctrl+Shift+D）')
  // …and the guide is there again under ？ (not by itself any more).
  await page.getByTestId('help').click()
  await expect(page.getByTestId('guide')).toBeVisible()
  await page.getByTestId('guide-close').click()

  // Administration: add a node. The write needs a fresh second factor. The
  // TOTP code used for enrolment is spent (one code, one use), so a recovery
  // code serves here.
  await page.getByRole('button', { name: '管理' }).click()
  await page.getByTestId('node-name').fill('this-pc')
  await page.getByTestId('node-create').click()
  await expect(page.getByText('再次验证')).toBeVisible()
  await page.getByPlaceholder('6 位动态码').fill(codes[0])
  await page.getByRole('button', { name: '确认' }).click()
  await expect(page.getByText('再次验证')).toBeHidden()
  const enrolLine = (await page.getByTestId('reveal').nth(1).textContent())!
  const token = /--token (\S+)/.exec(enrolLine)![1]
  expect(enrolLine).toContain('--pin ' + pin)
  await page.getByRole('button', { name: '已记下' }).click()

  // Enrol and start a real agent, exactly as the shown command says.
  const agentEnv = { ...process.env, LOCALAPPDATA: localAppData }
  execSync(`"${agentExe}" enroll --hub ${url} --token ${token} --pin ${pin}`, { env: agentEnv, stdio: 'pipe' })
  expect(existsSync(join(localAppData, 'termhub', 'agent.json'))).toBe(true)
  agent = spawn(agentExe, ['run', '-pipe', pipe], { env: agentEnv, stdio: 'ignore' })

  // A CLI profile from the blank template, running the test CLI directly.
  await page.getByRole('button', { name: 'CLI 配置' }).click()
  await page.getByRole('button', { name: '空白' }).click()
  await page.getByTestId('profile-name').fill('testcli')
  await page.getByLabel('启动方式').selectOption('direct')
  await page.getByTestId('profile-command').fill(testcli)
  await page.getByTestId('profile-save').click()
  await expect(page.getByRole('cell', { name: 'testcli', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '关闭' }).click()

  // The node comes online; the sidebar shows it.
  await expect(page.getByTestId('node')).toHaveCount(0) // the node list starts closed: 项目 lists the machines
  await page.getByTestId('nodes-toggle').click()
  await expect(page.getByTestId('node')).toContainText('this-pc')
  await expect(page.locator('[data-testid=node] .dot.on')).toBeVisible({ timeout: 30_000 })

  // New project: browse to a folder, create one, start the CLI inside it.
  projectRoot = mkdtempSync(join(tmpdir(), 'termhub-proj-'))
  await page.getByTestId('new-session').click()
  await page.getByLabel('工作目录').fill(projectRoot)
  await page.getByRole('button', { name: '浏览…' }).click()
  await expect(page.getByTestId('dir-list')).toBeVisible()
  await page.getByTestId('new-folder').click()
  await page.getByTestId('new-folder-name').fill('我的项目')
  await page.getByTestId('new-folder-ok').click()
  await expect(page.locator('.crumb').last()).toHaveText('我的项目')
  await page.getByTestId('pick-dir').click()
  await expect(page.getByLabel('工作目录')).toHaveValue(join(projectRoot, '我的项目'))
  // The lists are re-fetched on every session or node change; that must not
  // put the default folder back over the one just picked.
  await page.evaluate(() => (window as any).__thRefresh())
  await page.getByRole('button', { name: '浏览…' }).click()
  await page.getByLabel('显示隐藏').check() // must not jump back to where the picker opened
  await expect(page.locator('.crumb').last()).toHaveText('我的项目')
  await page.locator('.crumb').nth(-2).click()
  await page.getByLabel('显示隐藏').uncheck()
  await expect(page.getByTestId('dir-list')).toContainText('我的项目')
  await page.getByRole('button', { name: '取消' }).last().click()
  await expect(page.getByLabel('工作目录')).toHaveValue(join(projectRoot, '我的项目'))
  await page.getByTestId('start-session').click()

  // The terminal is live: the CLI announced itself, and it echoes what we type.
  await expect(page.locator('.xterm-screen')).toBeVisible()
  await expectTerm(page, 'READY')
  await page.locator('.xterm-helper-textarea').focus()
  await page.keyboard.type('echo 你好 termhub')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'ECHO "你好 termhub"')
  await expect(page.getByTestId('session')).toHaveCount(1)

  // Reload: the session is still there, its tab comes back from the saved
  // layout, and the page replays what happened.
  await page.reload()
  await expect(page.getByTestId('session')).toHaveCount(1)
  await expect(page.getByRole('tab')).toHaveCount(1)
  await expectTerm(page, 'ECHO "你好 termhub"')
  await expect(page.getByTestId('banner')).toHaveCount(0) // "restoring" went away on replay_end
  await page.locator('.xterm-helper-textarea').focus()
  await page.keyboard.type('echo after-reload')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'ECHO "after-reload"')

  // An image on the clipboard (what a screenshot tool leaves there): pasting
  // uploads it to the node and types its path into the terminal (docs/M4 第 7 节).
  await page.locator('.xterm-helper-textarea').focus()
  await page.evaluate(() => {
    const png = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='), c => c.charCodeAt(0))
    const dt = new DataTransfer()
    dt.items.add(new File([png], 'shot.png', { type: 'image/png' }))
    document.querySelector('.xterm-helper-textarea')!.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }))
  })
  await expectTerm(page, '.png')
  await page.keyboard.press('Enter')

  // A path in the output is a link: a click downloads the file; a relative
  // one is taken from the session's folder (docs/M9 第 8 节).
  writeFileSync(join(projectRoot, '我的项目', 'out.txt'), 'downloaded through a link')
  await page.locator('.xterm-helper-textarea').focus()
  await page.keyboard.type('echo out.txt')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'ECHO "out.txt"')
  const at = await page.evaluate(() => {
    const host = document.querySelector('.xterm-host') as any
    const t = host.__term, b = t.buffer.active
    for (let i = b.length - 1; i >= 0; i--) {
      const col = (b.getLine(i)?.translateToString(true) ?? '').indexOf('ECHO "out.txt"')
      if (col < 0) continue
      const r = host.querySelector('.xterm-screen').getBoundingClientRect()
      const w = r.width / t.cols, h = r.height / t.rows
      return { x: r.left + (col + 8) * w, y: r.top + (i - b.viewportY + 0.5) * h }
    }
    throw new Error('output line not found')
  })
  await page.mouse.move(at.x, at.y)
  const [dl] = await Promise.all([page.waitForEvent('download'), page.mouse.click(at.x, at.y)])
  expect(dl.suggestedFilename()).toBe('out.txt')
  expect(readFileSync(await dl.path(), 'utf8')).toBe('downloaded through a link')

  // Split: a second session in the second pane, then the selection of the
  // first terminal goes straight into the second (docs/M9 第 6 节, 验收 5).
  await page.getByTestId('split-row').click()
  await expect(page.getByTestId('pane')).toHaveCount(2)
  await expect(page.getByTestId('pane').nth(1)).toHaveClass(/focused/) // the new, empty pane
  await page.getByTestId('new-session').click()
  await page.getByLabel('工作目录').fill(projectRoot)
  await page.getByTestId('start-session').click()
  await expectTerm(page, 'READY', 1)
  await expect(page.getByTestId('session')).toHaveCount(2)
  await paneTerm(page, 0).focus()
  await selectLine(page, 0, 'echo after-reload')
  await page.getByTestId('pane').nth(0).getByTestId('send-other').click()
  await expectTerm(page, 'echo after-reload', 1) // typed into pane 2, not executed
  expect(await termText(page, 1)).not.toContain('ECHO "after-reload"')
  await paneTerm(page, 1).focus()
  await page.keyboard.press('Enter')
  await expectTerm(page, 'ECHO "after-reload"', 1)

  // The layout survives a reload: two panes, one tab each.
  await page.reload()
  await expect(page.getByTestId('pane')).toHaveCount(2)
  await expect(page.getByRole('tab')).toHaveCount(2)
  await expectTerm(page, 'ECHO "after-reload"', 1)

  // End the first session from the sidebar; its banner says so, the list
  // shrinks, and the tab stays open until closed by hand (docs/M9 第 8 节).
  page.once('dialog', d => d.accept())
  await page.getByTestId('session').filter({ hasText: '我的项目' }).getByTitle('结束会话').click()
  await expect(page.getByTestId('pane').nth(0).getByTestId('banner')).toContainText('会话已结束')
  await expect(page.getByTestId('session')).toHaveCount(1)
  await expect(page.getByRole('tab')).toHaveCount(2)
  await expect(page.getByRole('tab').first()).toContainText('已结束')
  await page.getByTitle('关闭标签（会话继续运行）').first().click()
  await expect(page.getByRole('tab')).toHaveCount(1)
  await page.getByTestId('unsplit').click()
  await expect(page.getByTestId('pane')).toHaveCount(1)
  await expect(page.getByRole('tab')).toHaveCount(1)

  // End the other one too.
  page.once('dialog', d => d.accept())
  await page.getByTitle('结束会话').click()
  await expect(page.getByTestId('banner')).toContainText('会话已结束')
  await expect(page.getByTestId('session')).toHaveCount(0)
  await page.getByTitle('关闭标签（会话继续运行）').click()
  await expect(page.getByRole('tab')).toHaveCount(0)

  // Logging out and back in with the trusted-device flow.
  // Nothing of the phone layout on a desktop (docs/M11): no key bar, box or
  // option cards, the terminal is writable, and a window dragged narrow keeps
  // the desktop layout with its split instead of tearing the terminals down.
  await expect(page.getByTestId('keybar')).toHaveCount(0)
  await expect(page.getByTestId('composer')).toHaveCount(0)
  await expect(page.getByTestId('options')).toHaveCount(0)
  await page.setViewportSize({ width: 640, height: 800 })
  await page.waitForTimeout(400)
  await expect(page.getByTestId('drawer-open')).toHaveCount(0)
  await expect(page.getByTestId('new-session')).toBeVisible()
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.getByTitle('退出登录').click()
  // Logout reloads after its request completes; do not race it with login's goto.
  await expect(page.getByLabel('用户名')).toBeVisible()
  await login(page)
  await page.getByLabel('验证器里的动态码，或一个恢复码').fill(codes[1])
  await page.getByRole('button', { name: '验证' }).click()
  await expect(page.getByTestId('new-session')).toBeVisible()
})

// History (docs/M10 第 3.2 节): the node reads a CLI's own conversation files
// (here fake Claude Code files under a CLAUDE_CONFIG_DIR of the test's own, so
// the machine's real history is never touched); projects in the sidebar, a
// read-only record, resuming with the conversation's id, hiding.
test('history: projects, record, resume, continue, hide', async ({ page }) => {
  const testcli = env('TH_TESTCLI')
  const claudeDir = mkdtempSync(join(tmpdir(), 'termhub-claude-'))
  histCwd = mkdtempSync(join(tmpdir(), 'termhub-hist-'))
  const otherCwd = mkdtempSync(join(tmpdir(), 'termhub-hist2-'))
  const conv = (dir: string, id: string, cwd: string, lines: object[]) => {
    mkdirSync(join(claudeDir, 'projects', dir), { recursive: true })
    writeFileSync(join(claudeDir, 'projects', dir, id + '.jsonl'), lines.map(l => JSON.stringify({ sessionId: id, ...l })).join('\n') + '\n')
  }
  const at = (min: number) => new Date(Date.now() - min * 60_000).toISOString()
  conv('p1', 'aaaaaaaa-1111-4000-8000-000000000001', histCwd, [
    { type: 'user', timestamp: at(90), cwd: histCwd, message: { role: 'user', content: '帮我写一个排序函数' } },
    { type: 'assistant', timestamp: at(89), cwd: histCwd, message: { role: 'assistant', content: [
      { type: 'text', text: '好的，下面是代码：\n```go\nfunc sortInts(a []int) { sort.Ints(a) }\n```' }, { type: 'tool_use', name: 'Edit', input: {} }] } },
    { type: 'ai-title', aiTitle: '排序函数' },
  ])
  conv('p1', 'aaaaaaaa-1111-4000-8000-000000000002', histCwd, [
    { type: 'user', timestamp: at(30), cwd: histCwd, message: { role: 'user', content: '再加上单元测试' } },
    { type: 'assistant', timestamp: at(29), cwd: histCwd, message: { role: 'assistant', content: [{ type: 'text', text: '已经加好了。' }] } },
  ])
  conv('p2', 'aaaaaaaa-1111-4000-8000-000000000003', otherCwd, [
    { type: 'user', timestamp: at(3 * 1440), cwd: otherCwd, message: { role: 'user', content: '另一个项目的问题' } },
  ])

  await login(page)
  await page.getByLabel('验证器里的动态码，或一个恢复码').fill(codes[3])
  await page.getByRole('button', { name: '验证' }).click()
  await expect(page.getByTestId('new-session')).toBeVisible()
  await page.getByTestId('guide-close').click() // a new browser: the guide opens once
  // A Claude Code profile that runs the test CLI; "--" ends the test CLI's own
  // flags, so it prints every argument the resume and continue commands add.
  const made = await page.evaluate(async ({ testcli, claudeDir, code }) => {
    const me = await (await fetch('/api/me')).json()
    await fetch('/api/auth/reverify', { method: 'POST', headers: { 'X-TH-CSRF': me.csrf, 'Content-Type': 'application/json' }, body: JSON.stringify({ code }) })
    const nodes = (await (await fetch('/api/nodes')).json()).nodes
    const r = await fetch('/api/admin/profiles', { method: 'POST', headers: { 'X-TH-CSRF': me.csrf, 'Content-Type': 'application/json' },
      body: JSON.stringify({ node_id: nodes[0].id, name: 'fakeclaude', kind: 'claude', mode: 'direct', command: testcli, args: ['-title', 'fake'],
        resume_cmd: `"${testcli}" -- --resume {session}`, continue_cmd: `"${testcli}" -- --continue`, env: { CLAUDE_CONFIG_DIR: claudeDir } }) })
    return r.status + ' ' + await r.text()
  }, { testcli, claudeDir, code: codes[4] })
  expect(made).toMatch(/^200 /)
  await page.reload()

  // Folders newest first, conversations newest first, how long ago.
  // By machine (docs/M10 第 3.5 节): closed, it shows its two newest
  // conversations with their folder; opened, its folders.
  const machine = page.getByTestId('machine')
  await expect(machine).toHaveCount(1)
  await expect(machine.locator('.mname')).toHaveText('this-pc')
  await expect(machine.getByTestId('conv')).toHaveText([/再加上单元测试.*29 分钟/, /排序函数.*1 小时/])
  await expect(machine.getByTestId('conv').nth(0)).toContainText(histCwd.split('\\').pop()!)
  await expect(machine).toContainText('全部 3 段对话')
  await machine.locator('.mrow').click()
  const projects = page.getByTestId('project')
  await expect(projects).toHaveCount(2)
  await expect(projects.nth(0)).toContainText(histCwd.split('\\').pop()!)
  await expect(projects.nth(0)).toContainText('29 分钟')
  await expect(projects.nth(1)).toContainText('3 天')
  await expect(projects.nth(0).getByTestId('conv')).toHaveText([/再加上单元测试\s*29 分钟/, /排序函数\s*1 小时/])

  // A read-only record first: nothing starts until asked.
  await projects.nth(0).getByTestId('conv').nth(1).click()
  const view = page.getByTestId('history-view')
  await expect(view).toContainText('排序函数')
  await expect(view.getByTestId('history-msg')).toHaveCount(2)
  await expect(view.locator('.user .bubble')).toHaveText('帮我写一个排序函数')
  await expect(view.locator('pre')).toContainText('func sortInts')
  await expect(view.locator('.tool')).toHaveText('Edit')
  await expect(page.getByTestId('session')).toHaveCount(0)

  // Resume: that conversation's id, in its folder, with the profile's own arguments.
  await view.getByTestId('history-resume').click()
  await expect(view).toHaveCount(0)
  await expectTerm(page, 'ARG 1="aaaaaaaa-1111-4000-8000-000000000001"')
  expect(await termText(page)).toContain('ARG 0="--resume"')
  await expect(page.getByTestId('session')).toHaveCount(1)
  // The list knows it is open: its dot, and the record offers to switch instead.
  await expect(projects.nth(0).getByTestId('conv').nth(1).locator('.live')).toHaveCount(1, { timeout: 15_000 })
  await projects.nth(0).getByTestId('conv').nth(1).click()
  await expect(view.getByTestId('history-resume')).toHaveText('切换到已打开的会话')
  await view.getByTestId('history-resume').click()
  await expect(page.getByTestId('session')).toHaveCount(1)

  // New session in that folder: "continue" is the default, and warns that a
  // session already runs there.
  await page.getByTestId('new-session').click()
  await page.getByLabel('CLI 配置').selectOption({ label: 'this-pc · fakeclaude' })
  await page.getByLabel('工作目录').fill(histCwd)
  await expect(page.getByTestId('mode-continue')).toHaveAttribute('aria-checked', 'true')
  await expect(page.locator('.modal .warn')).toContainText('已经有一个运行中的会话')
  await page.getByLabel('工作目录').fill(otherCwd + '\\nothing-here')
  await expect(page.getByTestId('mode-continue')).toBeDisabled()
  await expect(page.getByTestId('mode-new')).toHaveAttribute('aria-checked', 'true')
  await page.getByLabel('工作目录').fill(otherCwd)
  await page.getByTestId('mode-continue').click()
  await page.getByTestId('start-session').click()
  await expectTerm(page, 'ARG 0="--continue"')
  await expect(page.getByTestId('session')).toHaveCount(2)

  // Hiding changes only this list; the file stays; it can come back.
  // hide straight from the list, without opening it: ⋯ → 隐藏
  await projects.nth(0).getByTestId('conv').nth(0).hover()
  await projects.nth(0).getByTestId('conv-menu').nth(0).click()
  await page.getByTestId('menu-hide').click()
  await expect(projects.nth(0).getByTestId('conv')).toHaveCount(1)
  expect(existsSync(join(claudeDir, 'projects', 'p1', 'aaaaaaaa-1111-4000-8000-000000000002.jsonl'))).toBe(true)
  await page.getByTestId('history-hidden-toggle').click()
  await page.getByTestId('unhide').click()
  await expect(projects.nth(0).getByTestId('conv')).toHaveCount(2)
  // …and from inside the record
  await projects.nth(0).getByTestId('conv').nth(0).click()
  await view.getByTestId('history-hide').click()
  await expect(view).toHaveCount(0)
  await expect(projects.nth(0).getByTestId('conv')).toHaveCount(1)
  await page.getByTestId('unhide').click()
  await expect(projects.nth(0).getByTestId('conv')).toHaveCount(2)

  // A folder: renamed (the display only) and hidden with all its conversations, then back.
  page.once('dialog', d => d.accept('排序练习'))
  await projects.nth(0).getByTestId('folder-menu').click()
  await page.getByTestId('menu-rename').click()
  await expect(projects.nth(0).locator('.pname')).toHaveText('排序练习')
  expect(existsSync(histCwd)).toBe(true)
  page.once('dialog', d => d.accept())
  await projects.nth(0).getByTestId('folder-menu').click()
  await page.getByTestId('menu-hide-folder').click()
  await expect(projects).toHaveCount(1)
  await expect(page.getByTestId('unhide-folder')).toHaveCount(1) // the hidden list is still open from above
  await page.getByTestId('unhide-folder').click()
  await expect(projects).toHaveCount(2)
  page.once('dialog', d => d.accept('')) // empty: the folder's own name again
  await projects.nth(0).getByTestId('folder-menu').click()
  await page.getByTestId('menu-rename').click()
  await expect(projects.nth(0).locator('.pname')).toHaveText(histCwd.split('\\').pop()!)

  // Search filters by title, text or folder.
  await page.getByTestId('history-search-toggle').click()
  await page.getByTestId('history-search').fill('另一个')
  await expect(projects).toHaveCount(1)
  await page.getByTestId('history-search-toggle').click()
  await expect(projects).toHaveCount(2)

  // A folder range on the binding (docs/M10 第 3.4 节): only that folder's
  // conversations, a note saying so, and new sessions start there.
  const setRange = (folders: string[]) => page.evaluate(async ({ folders, code }) => {
    const me = await (await fetch('/api/me')).json()
    const h = { 'X-TH-CSRF': me.csrf, 'Content-Type': 'application/json' }
    if (code) await fetch('/api/auth/reverify', { method: 'POST', headers: h, body: JSON.stringify({ code }) })
    const p = (await (await fetch('/api/profiles')).json()).profiles.find((p: any) => p.name === 'fakeclaude')
    const r = await fetch(`/api/admin/profiles/${p.id}/bindings`, { method: 'PUT', headers: h,
      body: JSON.stringify({ user_ids: [me.user.id], folders: { [me.user.id]: folders } }) })
    return r.status
  }, { folders, code: folders.length ? codes[5] : '' })
  expect(await setRange([otherCwd])).toBe(200)
  await page.reload()
  await expect(projects).toHaveCount(1)
  await expect(projects.nth(0)).toContainText('另一个项目的问题')
  await expect(page.getByTestId('history-scope')).toContainText(otherCwd.split('\\').pop()!)
  await page.getByTestId('new-session').click()
  await page.getByLabel('CLI 配置').selectOption({ label: 'this-pc · fakeclaude' })
  await expect(page.getByLabel('工作目录')).toHaveValue(otherCwd)
  await page.getByRole('button', { name: '取消' }).click()
  expect(await setRange([])).toBe(200)
  await page.reload()
  await expect(projects).toHaveCount(2)

  // leave no sessions behind for the phone test
  await page.evaluate(async () => {
    const me = await (await fetch('/api/me')).json()
    for (const s of (await (await fetch('/api/sessions')).json()).sessions)
      await fetch('/api/sessions/' + s.sid, { method: 'DELETE', headers: { 'X-TH-CSRF': me.csrf } })
  })
  await expect(page.getByTestId('session')).toHaveCount(0)
})

// The same Hub from a phone (docs/M11): drawer, the input box, the key bar
// with sticky Ctrl, direct input, the installable shell.
test.describe('phone', () => {
  const { defaultBrowserType: _b, ...pixel } = devices['Pixel 7'] // the browser type must stay the project's
  test.use(pixel)

  test('drawer, input box, key bar, direct input, PWA shell', async ({ page }) => {
    await login(page)
    await page.getByLabel('验证器里的动态码，或一个恢复码').fill(codes[2])
    await page.getByRole('button', { name: '验证' }).click()
    // A new device: the guide again, on the 手机 tab; ⋮ → 使用帮助 reopens it.
    await expect(page.getByTestId('guide')).toContainText('长按')
    await page.getByTestId('guide-close').click()
    await page.getByTestId('more').click()
    await page.getByTestId('help').click()
    await expect(page.getByTestId('guide')).toBeVisible()
    await page.getByTestId('guide-close').click()
    await expect(page.getByTestId('drawer-open')).toBeVisible()
    // History on a phone: from the drawer, a conversation opens full screen,
    // with the resume button at the bottom; ‹ goes back.
    await page.getByTestId('drawer-open').click()
    await expect(page.getByTestId('machine').first()).toContainText('this-pc')
    await expect(page.getByTestId('conv').first()).toContainText(histCwd.split('\\').pop()!) // closed: the newest two, with their folder
    await expect(page.getByTestId('conv-menu').first()).toBeVisible() // ⋯ is always there on a touch screen
    await page.getByTestId('conv').first().locator('.cmain').click()
    await expect(page.getByTestId('history-view')).toBeVisible()
    await expect(page.locator('.drawer')).toHaveCount(0)
    await expect(page.getByTestId('history-view').getByTestId('history-resume')).toBeVisible()
    expect(await page.getByTestId('history-view').evaluate(e => e.getBoundingClientRect().width)).toBe(page.viewportSize()!.width)
    await page.getByRole('button', { name: '返回' }).click()
    await expect(page.getByTestId('history-view')).toHaveCount(0)
    // Android's back button closes what is on top, not the app (PWA 复核)
    await page.getByTestId('drawer-open').click()
    await page.getByTestId('conv').first().locator('.cmain').click()
    await expect(page.getByTestId('history-view')).toBeVisible()
    await page.goBack()
    await expect(page.getByTestId('history-view')).toHaveCount(0)
    await page.getByTestId('drawer-open').click()
    await expect(page.locator('.drawer')).toBeVisible()
    await page.goBack()
    await expect(page.locator('.drawer')).toHaveCount(0)
    await expect(page.getByTestId('drawer-open')).toBeVisible() // still in the app
    // turned sideways (wider than 768 px, but short): still the phone layout
    const upright = page.viewportSize()!
    await page.setViewportSize({ width: 915, height: 412 })
    await expect(page.getByTestId('drawer-open')).toBeVisible()
    await expect(page.locator('aside')).toHaveCount(0)
    await page.setViewportSize(upright)
    // The test CLI reads its input through the console's line editor, where
    // Esc clears the line and ↑ recalls history, so key bytes are checked on
    // the wire: binary input and the acknowledged input_batch JSON transport.
    const sent: string[] = []
    page.on('websocket', ws => {
      if (ws.url().includes('/ws/session/')) ws.on('framesent', f => { const data = sessionInput(f.payload); if (data) sent.push(data.toString('utf8')) })
    })

    // Installable shell: the manifest and the service worker are served as such.
    const mf = await page.request.get('/manifest.webmanifest')
    expect(mf.ok()).toBe(true)
    expect(mf.headers()['content-type']).toContain('manifest')
    expect((await mf.json()).name).toBe('termhub')
    const sw = await page.request.get('/sw.js')
    expect(sw.ok()).toBe(true)
    expect(await sw.text()).toContain('/assets/')
    const swState = await page.evaluate(() => navigator.serviceWorker.getRegistration().then(r => r ? (r.active?.state ?? r.installing?.state ?? 'registered') : 'none').catch(e => 'error: ' + e))
    console.log('service worker in the test browser:', swState)

    // A session from the drawer; on a phone the input box is the default.
    await page.getByTestId('drawer-open').click()
    await page.getByTestId('new-session').click()
    await page.getByLabel('工作目录').fill(projectRoot)
    await page.getByTestId('start-session').click()
    await expectTerm(page, 'READY')
    await expect(page.getByTestId('composer')).toBeVisible()
    await expect(page.getByTestId('keybar')).toBeVisible()
    await page.getByTestId('composer').fill('echo 来自输入框')
    await page.getByTestId('send-enter').click()
    await expectTerm(page, 'ECHO "来自输入框"')
    // A long press sends Esc first (interrupt), then the text and Enter.
    await page.getByTestId('composer').fill('echo 打断后发送')
    await page.getByTestId('send-enter').dispatchEvent('pointerdown')
    await page.waitForTimeout(800)
    await page.getByTestId('send-enter').dispatchEvent('pointerup')
    await expect.poll(() => sent.join('')).toContain('\x1becho 打断后发送\r')
    await expectTerm(page, 'ECHO "打断后发送"')

    // In box mode the terminal is read-only (a tap must not open the keyboard).
    await expect(page.locator('.xterm-helper-textarea')).toHaveJSProperty('readOnly', true)
    await expect(page.locator('.xterm-helper-textarea')).toHaveAttribute('inputmode', 'none')
    // Direct input: typing goes to the terminal. Key bar: Esc, Tab and an
    // arrow reach the program as the right bytes.
    await page.getByTestId('input-mode').click()
    await expect(page.getByTestId('composer')).toHaveCount(0)
    await expect(page.locator('.xterm-helper-textarea')).not.toHaveJSProperty('readOnly', true)
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type('echo ')
    const key = (k: string) => page.getByTestId('keybar').locator(`[data-key="${k}"]`)
    for (const k of ['Esc', 'Tab', '↑', '⏎']) await key(k).click()
    await expect.poll(() => sent.join('')).toContain('echo \x1b\t\x1b[A\r')

    // Sticky Ctrl: once, locked, off.
    await key('Ctrl').click(); await expect(key('Ctrl')).toHaveClass(/once/)
    await key('Ctrl').click(); await expect(key('Ctrl')).toHaveClass(/locked/)
    await key('Ctrl').click(); await expect(key('Ctrl')).not.toHaveClass(/once|locked/)

    // Ctrl (once) turns the next typed letter into a control byte.
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type('echo ')
    await key('Ctrl').click()
    await page.keyboard.type('a')
    await page.keyboard.press('Enter')
    await expect.poll(() => sent.join('')).toContain('echo \x01\r')

    // Option cards (docs/M11 第 6 节): menus drawn by the program become
    // tappable cards. A numbered menu sends the number; a cursor list sends
    // arrows and Enter; checkboxes send arrows and Space, then "提交" sends Enter.
    await page.keyboard.type('say ❯ 1. Yes|  2. Yes, allow all|  3. No')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('options')).toHaveAttribute('data-kind', 'numbered')
    await expect(page.getByTestId('option')).toHaveCount(3)
    await page.getByTestId('option').nth(1).dispatchEvent('click')
    await expect.poll(() => sent.join('')).toContain('\r2')
    await page.keyboard.press('Enter') // the test CLI reads lines: flush the digit it got
    // The real prompt on a narrow screen (Claude Code 2.1 on 49 columns): the
    // second choice wraps onto a deeper-indented row of its own, the question
    // sits above, the hint line below. Three cards, the title, Tab/Shift+Tab/Esc.
    await page.keyboard.type('say  Do you want to create a.txt?| > 1. Yes|   2. Yes, allow all edits during this session|      (shift+tab)|   3. No|| Esc to cancel · Tab to amend')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('option')).toHaveCount(3)
    await expect(page.getByTestId('option').nth(1)).toHaveText(/allow all edits during this session \(shift\+tab\)/)
    await expect(page.getByTestId('option-title')).toHaveText('Do you want to create a.txt?')
    await expect(page.getByTestId('option-extra')).toHaveText(['补充说明（Tab）', 'Shift+Tab', '取消（Esc）'])
    await page.getByTestId('option-extra').filter({ hasText: 'Esc' }).dispatchEvent('click')
    await expect.poll(() => sent.join('').endsWith('\x1b')).toBe(true)
    await page.keyboard.press('Enter')
    await page.keyboard.type('say ❯ Alpha|  Beta|  Gamma')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('options')).toHaveAttribute('data-kind', 'cursor')
    await page.getByTestId('option').nth(2).dispatchEvent('click')
    await expect.poll(() => sent.join('')).toContain('\x1b[B\x1b[B\r')
    await page.keyboard.type('say [ ] one|[x] two|[ ] three')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('options')).toHaveAttribute('data-kind', 'multi')
    await expect(page.getByTestId('option').nth(1)).toHaveClass(/checked/)
    await page.getByTestId('option').nth(2).dispatchEvent('click')
    await expect.poll(() => sent.join('')).toContain('\x1b[B\x1b[B ')
    await page.getByTestId('option-submit').dispatchEvent('click')
    await expect.poll(() => sent.join('').endsWith('\r')).toBe(true)
    // Claude Code's own multi-select question (captured from a real CLI): numbered
    // choices with checkboxes and a description line each, a "Submit" row,
    // "5. Chat about this" under a rule. A digit toggles; "提交" walks ↓ to
    // the Submit row and presses Enter.
    await page.keyboard.type('say ←  [ ] 颜色  √ Submit  →|你喜欢哪些颜色？|> 1. [ ] 红色|  暖色调，代表热情与活力|  2. [ ] 绿色|  自然色调|  3. [√] 蓝色|  冷色调|  4. [ ] Type something|     Submit|─────────────|  5. Chat about this||Enter to select · ↑/↓ to navigate · Esc to cancel')
    await page.keyboard.press('Enter')
    await expect(page.getByTestId('options')).toHaveAttribute('data-kind', 'multi')
    await expect(page.getByTestId('option')).toHaveCount(4)
    await expect(page.getByTestId('option').nth(0)).toHaveText(/1.*红色 暖色调，代表热情与活力/)
    expect((await import('../src/lib/menu')).splitKeys('\x1b[B\x1bOA\x1b[1;2Dab\x1b\r')).toEqual(['\x1b[B', '\x1bOA', '\x1b[1;2D', 'a', 'b', '\x1b', '\r'])
    // Paths in terminal output (docs/M9 第 8 节): what is found, what is not.
    const { findPaths, resolvePath } = await import('../src/lib/paths')
    const found = (s: string) => findPaths(s).map(h => h.text)
    const r = String.raw
    expect(found(r`● Write(C:\Users\me\Desktop\proj\report.md)`)).toEqual([r`C:\Users\me\Desktop\proj\report.md`])
    expect(found(r`文件已保存：C:\temp\图片.png。下一步`)).toEqual([r`C:\temp\图片.png`])
    expect(found(r`Read web/src/lib/paths.ts:12:3 and ./README.md, then ..\docs\M9.md`)).toEqual(['web/src/lib/paths.ts', './README.md', r`..\docs\M9.md`])
    expect(found(r`C:\proj\main.go(12,5): error`)).toEqual([r`C:\proj\main.go`])
    expect(found('see https://github.com/foo/bar.js, v2.1.186, 1.2.3, 10/20, e.g.')).toEqual([])
    expect(resolvePath('src/a.ts', r`C:\proj` + '\\')).toBe(r`C:\proj\src\a.ts`)
    expect(resolvePath('D:/x/y', r`C:\p`)).toBe(r`D:\x\y`)
    // Claude Code's agent switcher: radio dots, the pointer on either line.
    const { detectMenu } = await import('../src/lib/menu')
    for (const at of [0, 1]) {
      const rows = ['> hi', ' ↑/↓ to select', '', at ? '   ● main' : ' ❯ ● main', at ? ' ❯ ◯ opus-reviewer  Reading' : '   ◯ opus-reviewer  Reading']
      expect(detectMenu(rows)).toMatchObject({ kind: 'cursor', cursor: at, items: [{ label: '● main' }, { label: '◯ opus-reviewer  Reading' }] })
    }
    await expect(page.getByTestId('option').nth(2)).toHaveClass(/checked/)
    await expect(page.getByTestId('option-title')).toHaveText('你喜欢哪些颜色？')
    await expect(page.getByTestId('option-extra')).toHaveText(['聊聊这个（5）', '取消（Esc）'])
    await page.getByTestId('option').nth(1).dispatchEvent('click')
    await expect.poll(() => sent.join('').endsWith('2')).toBe(true)
    await page.getByTestId('option-submit').dispatchEvent('click')
    await expect.poll(() => sent.join('').endsWith('\x1b[B\x1b[B\x1b[B\x1b[B\r')).toBe(true)
    await page.keyboard.press('Enter')
    // ... and the review screen that follows is confirmed by itself
    await page.keyboard.type('say Review your answers|Ready to submit your answers?|> 1. Submit answers|  2. Cancel')
    await page.keyboard.press('Enter')
    await expect.poll(() => sent.join('').endsWith('1')).toBe(true)
    await page.keyboard.press('Enter')

    // Touch (docs/M11 第 7 节). xterm 6 brings no touch handling of its own:
    // a finger drag must scroll the history, a long press must select the
    // word under it with two handles, and the toolbar copies it.
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type('say ' + Array.from({ length: 80 }, (_, i) => 'line' + i).join('|')); await page.keyboard.press('Enter')
    await expectTerm(page, 'line79')
    const view = () => page.evaluate(() => { const t = (document.querySelector('.xterm-host') as any).__term; return { y: t.buffer.active.viewportY, base: t.buffer.active.baseY } })
    expect((await view()).base).toBeGreaterThan(0)
    const touch = (steps: [string, number, number][]) => page.evaluate(steps => {
      const host = document.querySelector('.xterm-host')!
      const mk = (x: number, y: number) => new Touch({ identifier: 1, target: host, clientX: x, clientY: y, pageX: x, pageY: y })
      for (const [type, x, y] of steps) host.dispatchEvent(new TouchEvent(type, { touches: type === 'touchend' ? [] : [mk(x, y)], changedTouches: [mk(x, y)], bubbles: true, cancelable: true }))
    }, steps)
    const drag: [string, number, number][] = [['touchstart', 150, 200]]
    for (let y = 210; y <= 500; y += 10) drag.push(['touchmove', 150, y])
    drag.push(['touchend', 150, 500])
    await touch(drag)
    await expect.poll(async () => { const v = await view(); return v.base - v.y }).toBeGreaterThan(5)
    // long press on the third row of text
    const cell = await page.evaluate(() => { const r = document.querySelector('.xterm-screen')!.getBoundingClientRect(); return { x: r.left + 12, y: r.top + 40 } })
    await touch([['touchstart', cell.x, cell.y]])
    await page.waitForTimeout(700)
    await touch([['touchend', cell.x, cell.y]])
    await expect(page.getByTestId('sel-handle')).toHaveCount(2)
    await expect.poll(() => page.evaluate(() => (document.querySelector('.xterm-host') as any).__term.getSelection())).toMatch(/^line\d+$/)
    await page.getByTestId('sel-copy').dispatchEvent('pointerdown')
    await expect(page.getByTestId('toast')).toContainText('已复制')
    await expect(page.getByTestId('sel-handle')).toHaveCount(0)
    expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/^line\d+$/)
    // ... or into the input box
    await touch([['touchstart', cell.x, cell.y]])
    await page.waitForTimeout(700)
    await touch([['touchend', cell.x, cell.y]])
    await expect(page.getByTestId('sel-handle')).toHaveCount(2)
    await page.getByTestId('sel-tobox').dispatchEvent('pointerdown')
    await expect(page.getByTestId('composer')).toHaveValue(/^line\d+$/)
    await page.getByTestId('input-mode').click() // back to direct input for the rest

    // The title opens the session list; "more" ends the session.
    await page.getByTestId('title').click()
    await expect(page.getByTestId('title-menu').getByRole('button')).toHaveCount(1)
    await page.getByTestId('title').click()
    page.once('dialog', d => d.accept())
    await page.getByTestId('more').click()
    await page.getByTestId('end-active').click()
    await expect(page.getByTestId('banner')).toContainText('会话已结束')

    // G4: saved input remains actionable without a running session or tab,
    // including direct-input mode and a reload. Only seed this test user's storage.
    await page.evaluate(async () => {
      const me = await (await fetch('/api/me')).json()
      localStorage.setItem('termhub.recovery.' + me.user.id, JSON.stringify({ 'review14-ended': 'G4 recovery text' }))
      localStorage.setItem('termhub.drafts.' + me.user.id, JSON.stringify({ 'review14-ended': 'own draft' }))
    })
    await page.reload()
    await page.getByTestId('drawer-open').click()
    const saved = page.getByTestId('saved-input')
    await saved.locator(':scope > summary').click()
    await saved.getByText('review14-ended', { exact: true }).click()
    await expect(saved.getByLabel('发送保留副本')).toHaveValue('G4 recovery text')
    page.once('dialog', d => d.accept())
    await saved.getByRole('button', { name: '放回草稿', exact: true }).click()
    await expect(saved.getByLabel('保存的草稿')).toHaveValue('G4 recovery text\nown draft')
    await expect(saved.getByLabel('发送保留副本')).toHaveCount(0)
    await saved.getByLabel('保存的草稿').fill('edited saved draft')
    await page.reload()
    await page.getByTestId('drawer-open').click()
    await saved.locator(':scope > summary').click()
    await saved.getByText('review14-ended', { exact: true }).click()
    await expect(saved.getByLabel('保存的草稿')).toHaveValue('edited saved draft')
  })
})

// Menu detection on screens seen on the phone (主人 2026-09-26): a Claude Code
// reply is not a menu; real menus still are.
import { test } from 'node:test'
import assert from 'node:assert/strict'

const { detectMenu } = await import('../../src/lib/menu.ts')

test('a reply starting with ● and its wrapped lines is not a menu', () => {
  const rows = [
    '  格式）。直接在本机 Claude Code',
    '  程序里搜这几个控制序列，只读，不运行它：',
    '',
    '  Ran 1 shell command',
    '',
    '● 需要确认 Claude Code 进入全屏界面后，是否',
    '  同时打开了鼠标报告（SGR 1006',
    '  格式）。直接在本机 Claude Code',
    '  程序里搜这几个控制序列，只读，不运行它：',
    '',
    '● T3 fw4 flush self-heal test · 34s',
    '  ⎿ $ ssh -i ~/.ssh/pve_ed25519',
    '',
    '✶ Stewing… (9m 30s · ↓ 28.3k tokens)',
  ]
  assert.equal(detectMenu(rows), null)
})

test('real menus are still found', () => {
  // the agent switcher: dots with a pointer
  const sw = ['> hi', ' ↑/↓ to select', '', ' ❯ ● main', '   ◯ opus-reviewer  Reading']
  assert.equal(detectMenu(sw)?.kind, 'cursor')
  // Claude Code's permission prompt
  const perm = ['Do you want to create a.txt?', '❯ 1. Yes', '  2. Yes, allow all edits during this session (shift+tab)', '  3. No, and tell Claude what to do differently (esc)']
  assert.equal(detectMenu(perm)?.kind, 'numbered')
})

// Claude Code's full-screen view pins the last message at the top as "> …";
// scrolled back, it and its wrapped lines read as a pointer and choices.
test('the pinned message at the top is not a menu', () => {
  const rows = [
    '> 1和2没必要改。3就关闭。你再review下，让…',
    '  连接因上游短暂卡顿超时（10.7',
    '  秒才成功），导致解析失败而非泄露，与 base',
    '  规则无关，我会记为观察项。接下来执行',
    '  T3，通过 fw4 flush 清空所有 nft 表，验证',
    '  watchdog 能否在 2 秒内恢复 base 和',
    '  guard，同时确认 185 始终断网。',
    '',
    '  Ran 1 shell command',
    '',
    '  T3通过：fw4 flush后watchdog在同一秒补装ba',
    '  se和guard，185全程断网而Gitea正常，恢复后',
    '  约4秒回到healthy，guard撤掉base保留。接下',
    '  来做T4，重启PC2并从日志时间线确认base和',
    '  guard早于dnsmasq、sing-box装上。',
    '',
    '  Ran 1 shell command',
    '',
    '  Ran 1 shell command',
    '',
    '  T4 通过验证：watchdog 优先启动，base/guard 早于',
    '  dnsmasq/sing-box 就绪，14秒后恢复。',
    '',
    '  Ran 1 shell command',
    '',
    '  全部测试完成，结果记入交接文档。',
  ]
  assert.equal(detectMenu(rows), null)
})

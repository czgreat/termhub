<script lang="ts">
  // Setup, login, the second factor and the forced first-login steps (docs/M6 第 3、4 节).
  import { onMount } from 'svelte'
  import { get, post } from '../lib/api'
  import { app, loadMe } from '../lib/state.svelte'
  import Mark from '../lib/Mark.svelte'
  import qrcode from 'qrcode-generator'

  // The otpauth link as a QR code, drawn locally (no network, no script): the
  // usual way to add an account to an authenticator app.
  function qrSvg(text: string): string {
    const qr = qrcode(0, 'M')
    qr.addData(text)
    qr.make()
    return qr.createSvgTag({ cellSize: 4, margin: 2, scalable: true })
  }

  let stage = $state<'loading' | 'setup' | 'login' | 'totp' | 'password' | 'enroll' | 'codes'>('loading')
  let username = $state(''), password = $state(''), token = $state(''), code = $state('')
  let ticket = $state(''), trust = $state(true), deviceName = $state('')
  let oldPw = $state(''), newPw = $state('')
  let secret = $state(''), uri = $state(''), recovery = $state<string[]>([])
  let error = $state(''), busy = $state(false)

  const titles = { loading: '', setup: '创建第一个管理员', login: '登录', totp: '两步验证', password: '设置新密码', enroll: '绑定验证器', codes: '保存恢复码' }
  const steps = { password: 1, enroll: 2, codes: 3 } as Record<string, number>

  onMount(async () => {
    if (app.me) { next(); return }
    const s = await get('/api/setup')
    stage = s.needed ? 'setup' : 'login'
  })

  function next() {
    const p = app.me?.pending ?? []
    if (p.includes('change_password')) stage = 'password'
    else if (p.includes('enroll_totp')) stage = 'enroll'
    else stage = 'login'
  }

  async function run(fn: () => Promise<void>) {
    error = ''; busy = true
    try { await fn() } catch (e: any) { error = e?.message ?? String(e) } finally { busy = false }
  }

  const setup = () => run(async () => {
    await post('/api/setup', { token, username, password })
    stage = 'login'
  })
  const login = () => run(async () => {
    const r = await post('/api/auth/login', { username, password })
    if (r.done) { await loadMe(); next(); return }
    ticket = r.ticket
    deviceName = navigator.userAgent.includes('Mobile') ? '手机' : '电脑'
    stage = 'totp'
  })
  const totp = () => run(async () => {
    await post('/api/auth/totp', { ticket, code, trust, deviceName })
    code = ''
    await loadMe(); next()
  })
  const changePw = () => run(async () => {
    await post('/api/me/password', { old: oldPw, new: newPw })
    await loadMe(); next()
  })
  const beginEnroll = () => run(async () => {
    const r = await post('/api/me/totp/begin')
    secret = r.secret; uri = r.uri
  })
  const confirmEnroll = () => run(async () => {
    const r = await post('/api/me/totp/confirm', { code })
    recovery = r.recovery_codes; code = ''
    stage = 'codes'
  })
  const finish = () => run(async () => { await loadMe(); next() })
</script>

<div class="wrap">
  <div class="glow a"></div>
  <div class="glow b"></div>
  <div class="box">
    <div class="brand">
      <Mark size={44} />
      <div>
        <div class="name">termhub</div>
        <div class="tag">在浏览器里操作各台 Windows 上的 Claude Code 与 Codex</div>
      </div>
    </div>

    {#if stage === 'loading'}
      <p class="muted">加载中…</p>
    {:else}
      <div class="stage-head">
        <h2>{titles[stage]}</h2>
        {#if app.authExpired && stage === 'login'}<p class="expired" data-testid="auth-expired">登录已过期，请重新登录。</p>{/if}
        {#if steps[stage]}<span class="steps">首次登录 {steps[stage]} / 3</span>{/if}
      </div>
    {/if}

    {#if stage === 'setup'}
      <p class="hint">还没有任何用户。用容器日志里打印的一次性初始化口令创建第一个管理员。</p>
      <form onsubmit={e => { e.preventDefault(); setup() }}>
        <label for="tok">初始化口令</label><input id="tok" bind:value={token} required />
        <label for="u">管理员用户名</label><input id="u" bind:value={username} required />
        <label for="p">密码（至少 11 位）</label><input id="p" type="password" bind:value={password} required />
        <button class="primary go" disabled={busy}>创建管理员</button>
      </form>
    {:else if stage === 'login'}
      <form onsubmit={e => { e.preventDefault(); login() }}>
        <label for="u">用户名</label><input id="u" bind:value={username} autocomplete="username" required />
        <label for="p">密码</label><input id="p" type="password" bind:value={password} autocomplete="current-password" required />
        <button class="primary go" disabled={busy}>{busy ? '登录中…' : '登录'}</button>
      </form>
    {:else if stage === 'totp'}
      <p class="hint">打开验证器应用，输入 termhub 这一项当前显示的 6 位数字。</p>
      <form onsubmit={e => { e.preventDefault(); totp() }}>
        <label for="c">验证器里的动态码，或一个恢复码</label>
        <input id="c" class="code" bind:value={code} autocomplete="one-time-code" inputmode="numeric" required />
        <label class="check"><input type="checkbox" bind:checked={trust} /> 30 天内信任此设备，不再要求动态码</label>
        <button class="primary go" disabled={busy}>验证</button>
      </form>
    {:else if stage === 'password'}
      <p class="hint">请把临时密码换成你自己的密码（至少 11 位）。</p>
      <form onsubmit={e => { e.preventDefault(); changePw() }}>
        <label for="o">当前密码</label><input id="o" type="password" bind:value={oldPw} required />
        <label for="n">新密码</label><input id="n" type="password" bind:value={newPw} required />
        <button class="primary go" disabled={busy}>修改密码</button>
      </form>
    {:else if stage === 'enroll'}
      <p class="hint">绑定一个验证器应用（如 Aegis、Authy、微软验证器），以后登录时要输入它显示的动态码。</p>
      {#if !secret}
        <button class="primary go" onclick={beginEnroll} disabled={busy}>生成密钥</button>
      {:else}
        <div class="qr-row">
          <div class="qr" data-testid="totp-qr">{@html qrSvg(uri)}</div>
          <div class="qr-text">
            <p>用验证器扫这个二维码。</p>
            <p class="muted small">扫不了就手动添加，密钥：<code data-testid="totp-secret">{secret}</code></p>
          </div>
        </div>
        <form onsubmit={e => { e.preventDefault(); confirmEnroll() }}>
          <label for="c">输入验证器显示的 6 位动态码以确认</label>
          <input id="c" class="code" bind:value={code} autocomplete="one-time-code" inputmode="numeric" required />
          <button class="primary go" disabled={busy}>确认绑定</button>
        </form>
      {/if}
    {:else if stage === 'codes'}
      <p class="hint">验证器已绑定。下面是 10 个恢复码，<b>只显示这一次</b>，请保存好。验证器丢失时每个码可用一次。</p>
      <pre class="codes" data-testid="recovery-codes">{recovery.join('\n')}</pre>
      <button class="primary go" onclick={finish}>我已保存，进入</button>
    {/if}
    {#if error}<p class="err box-err">{error}</p>{/if}
  </div>
  <p class="foot muted">termhub {__TH_VERSION__}</p>
</div>

<style>
  .wrap { position: relative; height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 16px; overflow: hidden; background: radial-gradient(1200px 600px at 50% -10%, #182235 0%, var(--bg) 60%); }
  .glow { position: absolute; border-radius: 50%; filter: blur(80px); opacity: .35; pointer-events: none; }
  .glow.a { width: 420px; height: 420px; background: var(--accent); left: -120px; top: -120px; }
  .glow.b { width: 380px; height: 380px; background: var(--accent-2); right: -120px; bottom: -140px; }
  .box { position: relative; width: min(440px, 100%); background: var(--panel); border: 1px solid var(--line); border-radius: 16px; padding: 26px 28px 22px; box-shadow: var(--shadow); }
  .brand { display: flex; align-items: center; gap: 14px; margin-bottom: 18px; }
  .name { font-size: 22px; font-weight: 700; letter-spacing: .3px; }
  .tag { color: var(--dim); font-size: 12px; margin-top: 2px; }
  .stage-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; margin: 4px 0 2px; }
  h2 { margin: 0; font-size: 17px; font-weight: 600; }
  .steps { font-size: 12px; color: var(--accent); }
  .hint { color: var(--dim); font-size: 13px; margin: 6px 0 4px; }
  .go { width: 100%; margin-top: 18px; padding: 10px 12px; font-size: 15px; }
  .code { font-size: 20px; letter-spacing: .25em; font-family: ui-monospace, Consolas, monospace; text-align: center; }
  .check { display: flex; align-items: center; gap: 8px; margin-top: 12px; font-size: 13px; color: var(--fg); }
  .check input { width: auto; margin: 0; }
  .qr-row { display: flex; gap: 14px; align-items: center; margin: 8px 0; }
  .qr { flex: none; width: 160px; height: 160px; background: #fff; padding: 6px; border-radius: 8px; }
  .qr :global(svg) { width: 100%; height: 100%; display: block; }
  .qr-text p { margin: 4px 0; }
  .small { font-size: 12px; }
  .codes { white-space: pre; font-family: ui-monospace, Consolas, monospace; background: var(--bg); border: 1px solid var(--line); border-radius: 8px; padding: 12px 14px; margin: 8px 0 0; column-count: 2; column-gap: 24px; font-size: 14px; }
  .box-err { margin: 12px 0 0; padding: 8px 10px; background: rgba(229, 83, 75, .12); border: 1px solid rgba(229, 83, 75, .4); border-radius: 8px; font-size: 13px; }
  .foot { position: relative; margin: 14px 0 0; font-size: 11px; }
  @media (max-width: 480px) { .box { padding: 22px 18px 18px; } .qr-row { flex-direction: column; align-items: flex-start; } }
  .expired { color: var(--warn); font-size: 13px; margin: 4px 0 0; width: 100%; }
</style>

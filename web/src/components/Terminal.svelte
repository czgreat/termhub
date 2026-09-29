<script lang="ts">
  // The terminal itself (docs/M9 第 4 到 8 节): xterm.js on one session link,
  // with resume/replay, size leadership, copy/paste and file/image paste.
  import { onMount, untrack } from 'svelte'
  import { Terminal } from '@xterm/xterm'
  import { FitAddon } from '@xterm/addon-fit'
  import { WebLinksAddon } from '@xterm/addon-web-links'
  import { UnicodeGraphemesAddon } from '@xterm/addon-unicode-graphemes'
  import { ClipboardAddon } from '@xterm/addon-clipboard'
  import { WebglAddon } from '@xterm/addon-webgl'
  import { SessionLink, type Attached, type Ctl } from '../lib/session'
  import { post, uploadChunks } from '../lib/api'
  import { app, toast, refreshLists, sessionOf, replaceTab, focusPane, otherPane, applyMods, backgroundGrace, setInputMode } from '../lib/state.svelte'
  import { terms } from '../lib/terms'
  import { withReverify } from '../lib/reverify.svelte'
  import { detectMenu } from '../lib/menu'
  import { pathLinkProvider, resolvePath } from '../lib/paths'
  import { downloadPath } from '../lib/download'
  import { attachTouch, cellPos, cellSize, screenOrigin, type SelectionState, type TouchHandle } from '../lib/touch'

  let { sid, pane = 0, visible, attached = true, onsend }: { sid: string; pane?: number; visible: boolean; attached?: boolean; onsend?: () => void } = $props()
  let el: HTMLDivElement
  let term: Terminal
  let fit: FitAddon
  let capturedPaste: string[] | null = null
  let link: SessionLink
  let webgl: WebglAddon | null = null

  let banner = $state(''), bannerKind = $state<'info' | 'warn' | 'bad'>('info')
  let peers = $state(1), sizeBy = $state(''), exited = $state<null | { reason: string; exit_code: number | null }>(null)
  let progress = $state(''), replaying = $state(false), retries = $state(0)
  let follows = $state(false) // size is decided by another device
  // 回到底部 (主人 09-26): shown once the view is scrolled away from the latest
  // output. A normal screen says where its view is; a full-screen program
  // (Claude Code) scrolls itself, so the lines scrolled up are counted and
  // typing counts as being back at the bottom (it jumps there itself).
  let scrolledUp = $state(false), altUp = $state(0), newBelow = $state(false)
  const awayFromBottom = $derived(scrolledUp || altUp > 0)
  // back at the bottom: the live prompt, if any, is read again
  $effect(() => { if (!awayFromBottom && term) untrack(scanMenu) })
  // One person at the keyboard: someone else's session only watched until
  // taken over; the owner watches while an administrator has it (takenBy).
  let readOnly = $state(false), takenBy = $state('')
  const mine = $derived(sessionOf(sid)?.owner_id === app.me?.user.id)
  let altScreen = false
  let cols = $state(0), rows = $state(0)
  let hasSelection = $state(false)
  // touch selection (docs/M11 第 7 节): the two handles and the little toolbar
  let touch: TouchHandle | null = null
  let sel = $state<SelectionState | null>(null)
  let handles = $state<{ start: { x: number; y: number } | null; end: { x: number; y: number } | null }>({ start: null, end: null })
  function placeHandles(s: SelectionState | null) {
    sel = s
    if (!s) { handles = { start: null, end: null }; return }
    const first = s.start.row < s.end.row || (s.start.row === s.end.row && s.start.col <= s.end.col)
    const [a, b] = first ? [s.start, s.end] : [s.end, s.start]
    const o = screenOrigin(term), h = el.getBoundingClientRect(), c = cellSize(term)
    const at = (cell: typeof a, right: boolean) => { const p = cellPos(term, cell); return p && { x: p.x + (right ? c.w : 0) + o.left - h.left, y: p.y + c.h + o.top - h.top } }
    handles = first ? { start: at(a, false), end: at(b, true) } : { start: at(b, true), end: at(a, false) }
  }
  async function copySelection() {
    const text = term.getSelection()
    try { await navigator.clipboard.writeText(text); toast('已复制') } catch { toast('复制失败：浏览器不允许写剪贴板') }
    touch?.clear()
  }
  // The selected text as a path: long-pressing a path selects it whole
  // (backslash and colon do not split words); quotes around it are dropped.
  const selPath = $derived.by(() => {
    if (!sel) return ''
    const t = term.getSelection().trim().replace(/^["'`]+|["'`]+$/g, '')
    return t && t.length < 1000 && !/[\r\n]/.test(t) && /[\\/.]/.test(t) ? t : '' // a path has a separator or an extension
  })
  function download(p: string) {
    if (!session) return
    downloadPath(session.node_id, resolvePath(p, session.cwd))
  }
  function selectionDownload() {
    const p = selPath
    touch?.clear()
    if (p) download(p)
  }
  // xterm's own mouse reports (a wheel over Claude Code) are not typing
  const ESC = String.fromCharCode(27)
  const isMouseReport = (d: string) => d.startsWith(ESC + '[<') || d.startsWith(ESC + '[M')
  function typed() { altUp = 0; if (!scrolledUp) newBelow = false }
  function toBottom() {
    if (term.buffer.active.type !== 'alternate') term.scrollToBottom()
    // Claude Code's full-screen view binds Ctrl+End to scroll:bottom (its own
    // key table, context "Scroll"); counted wheel notches fell short, as it
    // speeds up quick wheels (主人 09-26: 回不到底部)
    else if (session?.kind === 'claude') link.input(ESC + '[1;5F')
    else touch?.wheelDown(Math.min(altUp, 500))
    altUp = 0; newBelow = false; scrolledUp = false
  }
  function selectionToBox() {
    const text = term.getSelection()
    app.drafts[sid] = (app.drafts[sid] ?? '') + text
    if (app.inputMode !== 'box') setInputMode('box')
    touch?.clear()
  }

  const me = app.me?.user.username ?? ''
  const session = $derived(sessionOf(sid))
  const node = $derived(app.nodes.find(n => n.id === session?.node_id))
  const canSend = $derived(hasSelection && otherPane() >= 0 && app.focus === pane)

  function b64(s: string) {
    const bin = atob(s)
    const out = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
    return out
  }

  const readOnlyText = '只读：这个会话现在由别人操作，输入没有发出'
  /** Takes someone else's session over (a second factor each time), or takes one's own back. */
  async function take() {
    try { await withReverify(() => post(`/api/sessions/${sid}/take`, {})) } catch (e: any) { toast(e.message) }
  }

  function doFit() {
    if (!visible) return
    if (readOnly) return // a watcher shows the typist's size
    fit.fit()
    if (!follows && (term.cols !== link.cols || term.rows !== link.rows)) link.resize(term.cols, term.rows)
    cols = term.cols; rows = term.rows
  }

  // The visible rows are read for a menu after output settles, so the phone's
  // option cards follow the screen (docs/M11 第 6 节) and the status dots can
  // tell a session that asks from one that has finished (状态点复核 1). A CLI redraws its
  // screen in pieces; a menu that vanishes only for a moment must not take
  // the cards with it, so clearing waits a little longer than finding.
  let menuTimer: number | undefined, menuGone: number | undefined
  function scanMenu() {
    clearTimeout(menuTimer)
    // looking back through history: what is on the screen is not being asked now
    if (awayFromBottom) { clearTimeout(menuGone); menuGone = undefined; if (app.menus[sid]) app.menus[sid] = null; return }
    menuTimer = window.setTimeout(() => {
      const b = term.buffer.active
      const rows: string[] = []
      for (let i = 0; i < term.rows; i++) {
        const line = b.getLine(b.viewportY + i)
        const text = line?.translateToString(true) ?? ''
        // a narrow screen wraps long menu lines: put the pieces back together
        if (line?.isWrapped && rows.length) rows[rows.length - 1] += text
        else rows.push(text)
      }
      const m = detectMenu(rows)
      const cur = app.menus[sid]
      if (m) { clearTimeout(menuGone); menuGone = undefined }
      if (m && JSON.stringify(m) !== JSON.stringify(cur ?? null)) app.menus[sid] = m
      else if (!m && cur && menuGone === undefined) menuGone = window.setTimeout(() => { menuGone = undefined; app.menus[sid] = null }, 700)
    }, 150)
  }

  // In the phone's "输入框" mode the terminal is read-only: a tap on it must
  // not open the keyboard, all writing goes through the box (docs/M11 第 5 节).
  function applyReadOnly() {
    const ro = readOnly || (app.mobile && app.inputMode === 'box') // watching only: no keyboard either
    const ta = term?.textarea
    if (!ta) return
    ta.readOnly = ro
    ta.setAttribute('inputmode', ro ? 'none' : 'text')
    if (ro && document.activeElement === ta) ta.blur()
  }
  $effect(applyReadOnly) // the terminal itself is not reactive: also called once it exists

  // WebGL only while in view: browsers cap the number of contexts (docs/M9 第 4 节).
  function setWebgl(on: boolean) {
    if (on && !webgl) {
      try {
        webgl = new WebglAddon()
        webgl.onContextLoss(() => { webgl?.dispose(); webgl = null })
        term.loadAddon(webgl)
      } catch { webgl = null }
    } else if (!on && webgl) {
      webgl.dispose(); webgl = null
    }
  }

  onMount(() => {
    term = new Terminal({ cursorBlink: true, scrollback: 10000, allowProposedApi: true, fontSize: 14,
      fontFamily: '"Cascadia Mono", Consolas, "Microsoft YaHei Mono", "Noto Sans Mono CJK SC", ui-monospace, monospace',
      theme: { background: '#1b1f24' }, scrollOnEraseInDisplay: true })
    fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new WebLinksAddon((e, url) => window.open(url, '_blank', 'noopener,noreferrer')))
    // File paths in the output download on click (docs/M9 第 8 节).
    term.registerLinkProvider(pathLinkProvider(term, p => download(p), t => { if (term.element) term.element.title = t ? '点击下载 ' + t : '' }))
    term.loadAddon(new UnicodeGraphemesAddon())
    term.unicode.activeVersion = '15-graphemes'
    term.loadAddon(new ClipboardAddon())
    term.open(el)
    ;(el as any).__term = term // for tests: the WebGL renderer leaves no text in the DOM
    applyReadOnly()
    setWebgl(visible)

    link = new SessionLink(sid, {
      attached(a: Attached) {
        replaying = a.mode === 'replay'
        altScreen = !!a.alt_screen
        if (a.mode === 'replay') {
          term.reset()
          if (a.cols && a.rows) term.resize(a.cols, a.rows)
          if (a.prelude) term.write(b64(a.prelude))
        }
        banner = ''; exited = null; retries = 0
      },
      output(data) {
        term.write(data)
        if (!visible && !replaying) app.marks[sid] ??= 'output'
        if (awayFromBottom && !replaying) newBelow = true
        scanMenu()
      },
      control(m: Ctl) {
        switch (m.t) {
          case 'replay_end': // the replay is out; live output follows (docs/M1 4.2)
            if (replaying) { replaying = false; if (altScreen) link.redraw() }
            break
          case 'size':
            sizeBy = m.by
            follows = m.by !== me && m.by !== '管理员 ' + me && m.by !== ''
            if (follows) term.resize(m.cols, m.rows)
            else doFit()
            break
          case 'peers': peers = m.count; break
          case 'driver':
            readOnly = !m.on; takenBy = m.by ?? ''
            if (readOnly) { follows = true; if (m.cols && m.rows) term.resize(m.cols, m.rows); cols = term.cols; rows = term.rows }
            else if (visible) { follows = false; fit.fit(); link.resize(term.cols, term.rows); cols = term.cols; rows = term.rows } // at the keyboard again: this window's size at once
            break
          case 'input_readonly': toast(readOnlyText); break
          case 'exited':
            exited = { reason: m.reason, exit_code: m.exit_code ?? null }; bannerKind = 'warn'; replaying = false
            link.close() // nothing more will come; do not reconnect to an ended session
            break
          case 'node_offline': banner = '节点离线，会话状态未知'; bannerKind = 'bad'; break
          case 'node_online': banner = ''; link.attach(); break
          case 'err':
            if (m.code === 'node_offline') { banner = '节点离线，输入未送达'; bannerKind = 'bad' }
            else if (m.code === 'read_only') toast(readOnlyText)
            else if (m.code === 'not_running') { exited = { reason: 'ended', exit_code: null }; replaying = false; banner = ''; toast('会话已结束') }
            break
          case 'input_dropped':
            if (m.partial) askUnblock()
            else toast('连接中断太久（或积压太多），这段时间的输入都没有发出，请重新输入')
            break
          case 'input_blocked': askUnblock(); break
        }
      },
      state(s) {
        app.linkState[sid] = s
        if (s === 'connecting') { banner = retries ? `重连中…（第 ${retries} 次）` : '连接中…'; bannerKind = 'info' }
        else if (s === 'closed' && !exited) { retries++; banner = '连接已断开，正在重连…'; bannerKind = 'warn' }
        else if (s === 'detached') { banner = ''; }
      },
    })
    doFit()
    link.cols = term.cols; link.rows = term.rows
    if (attached) link.connect()

    // Watching only: xterm's own answers (cursor reports, focus, mouse) are
    // dropped without a word; a key pressed says why (keydown below).
    term.onData(d => { if (capturedPaste) { capturedPaste.push(d); return }; if (readOnly) return; follows = false; if (!isMouseReport(d)) typed(); link.input(applyMods(d)) })
    const where = () => { const b = term.buffer.active; scrolledUp = b.type === 'normal' && b.viewportY < b.baseY; if (!awayFromBottom) newBelow = false }
    term.onScroll(where)
    term.onWriteParsed(where)
    term.buffer.onBufferChange(() => { altUp = 0; where() })
    // a real mouse wheel over a full-screen program with mouse reports (the drag's own notches are synthetic)
    el.addEventListener('wheel', e => {
      if (!e.isTrusted || term.buffer.active.type !== 'alternate' || term.modes.mouseTrackingMode === 'none') return
      altUp = Math.max(0, altUp + (e.deltaY < 0 ? 1 : -1) * Math.max(1, Math.round(Math.abs(e.deltaMode ? e.deltaY : e.deltaY / 40))))
      if (!awayFromBottom) newBelow = false
    }, { capture: true, passive: true })
    term.onBinary(d => { if (!readOnly) link.input(Uint8Array.from(d, c => c.charCodeAt(0))) })
    term.onBell(() => { if (!visible) app.marks[sid] = 'bell' })
    term.onSelectionChange(() => hasSelection = term.hasSelection())
    // Ctrl+C copies when there is a selection, interrupts otherwise (docs/M9 第 5 节).
    // Ctrl+Shift+letter belongs to the page frame (App.svelte), not to the terminal.
    term.attachCustomKeyEventHandler(e => {
      if (e.type !== 'keydown') return true
      if (e.ctrlKey && !e.shiftKey && e.key === 'c' && term.hasSelection()) {
        navigator.clipboard.writeText(term.getSelection()); term.clearSelection(); return false
      }
      if (e.ctrlKey && e.shiftKey && e.key === 'C') { navigator.clipboard.writeText(term.getSelection()); return false }
      if (readOnly && !(e.ctrlKey && e.shiftKey) && !['Shift', 'Control', 'Alt', 'Meta', 'CapsLock'].includes(e.key)) { toast(readOnlyText); return false }
      // Ctrl+V / Ctrl+Shift+V paste (docs/M9 第 5 节). Left to xterm, Ctrl+V becomes
      // the control byte 0x16 and the browser never raises a paste event, so
      // images (which only arrive through that event) would be lost.
      if (e.ctrlKey && !e.altKey && e.key.toLowerCase() === 'v') return false
      if (e.ctrlKey && e.shiftKey && !e.altKey && /^[A-Za-z]$/.test(e.key) && e.key.toUpperCase() !== 'V') return false
      return true
    })
    terms.set(sid, {
      pasteBatch: async text => {
        capturedPaste = []
        let data = ''
        try { term.paste(text); data = capturedPaste.join('') } finally { capturedPaste = null }
        return link.inputBatch(data)
      },
      inputBatch: text => { typed(); return link.inputBatch(applyMods(text)) },
      canSend: () => link.canSend, refuses: () => link.refuses, drops: () => link.drops, readOnly: () => link.readOnly,
      selection: () => term.getSelection(), paste: t => term.paste(t), focus: () => term.focus(),
      input: d => { typed(); link.input(applyMods(d)) }, files: pasteFiles, appCursor: () => term.modes.applicationCursorKeysMode,
    })
    term.textarea?.addEventListener('focus', () => focusPane(pane))
    touch = attachTouch({ term, host: el, send: k => link.input(k), onSelection: placeHandles, // reacts to touches only
      arrowsOk: () => session?.kind !== 'claude' && session?.kind !== 'codex',
      onAltScroll: up => { altUp = Math.max(0, altUp + up); if (!awayFromBottom) newBelow = false } })

    // A phone's keyboard changes the height on every show/hide; the size
    // report is debounced longer there (docs/M11 第 4 节).
    const ro = new ResizeObserver(() => { clearTimeout(fitTimer); fitTimer = window.setTimeout(doFit, app.mobile ? 300 : 100) })
    let fitTimer: number | undefined
    ro.observe(el)
    const online = () => { if (attached) link.poke() }
    // In the background for long the link is let go; back in front it resumes (docs/M11 第 8 节).
    let bgTimer: number | undefined
    let hiddenAt = 0
    const vis = () => {
      clearTimeout(bgTimer)
      if (document.visibilityState === 'visible') {
        // iOS freezes timers in the background, so the detach below may never
        // have run and the link may be dead while it still looks open: after a
        // long absence it is made again, by the clock (PWA 复核).
        if (hiddenAt && Date.now() - hiddenAt > backgroundGrace && !exited) link.detach()
        hiddenAt = 0
        if (attached) link.poke()
      } else {
        hiddenAt = Date.now()
        if (app.mobile) bgTimer = window.setTimeout(() => { if (!exited) link.detach() }, backgroundGrace)
      }
    }
    window.addEventListener('online', online)
    document.addEventListener('visibilitychange', vis)
    // Capture phase: xterm's own paste handler on its textarea stops
    // propagation, so a listener on the parent would never see an image.
    el.addEventListener('paste', onPaste, true)
    el.addEventListener('drop', onDrop, true)
    el.addEventListener('dragover', e => e.preventDefault())
    return () => {
      clearTimeout(bgTimer); clearTimeout(menuTimer); clearTimeout(menuGone); touch?.detach(); ro.disconnect(); link.close(); term.dispose(); terms.delete(sid); delete app.linkState[sid]; delete app.menus[sid]
      window.removeEventListener('online', online); document.removeEventListener('visibilitychange', vis)
    }
  })

  // Only the change of `visible` matters here; nothing else this reads may
  // re-run it (a re-run would steal the focus).
  let wasVisible = false
  $effect(() => {
    const v = visible
    untrack(() => {
      // On a phone in "input box" mode the terminal is not focused by itself:
      // that would pop the soft keyboard up (docs/M11 第 5 节).
      if (v && term) { setWebgl(true); doFit(); if (!wasVisible && !(app.mobile && app.inputMode === 'box')) term.focus(); delete app.marks[sid] }
      else if (term) setWebgl(false)
      wasVisible = v
    })
  })

  // Beyond the mount limit the tab keeps its screen but drops its connection;
  // back in favour it resumes from `have` (docs/M9 第 3 节).
  $effect(() => { if (!link || exited) return; if (attached) link.poke(); else link.detach() })

  // The session ended while this tab could not hear it (the list says so but
  // no `exited` arrived, e.g. it ended during a disconnect): show that instead
  // of reconnecting forever to a session the Hub no longer knows.
  $effect(() => {
    const s = app.ended[sid]
    if (!s || exited) return
    const t = window.setTimeout(() => {
      if (exited) return
      exited = { reason: s.end_reason || 'ended', exit_code: s.exit_code ?? null }; bannerKind = 'warn'; replaying = false; banner = ''
      link.close()
    }, 3000)
    return () => clearTimeout(t)
  })

  // Image or file in the clipboard / dropped: upload to the node, then type
  // the returned path (docs/M4 第 7 节, M9 第 7 节).
  // A queue dropped half sent (第二轮复核 2): the start of a command may be on
  // the command line already, and the next Enter would run it torn. Input
  // waits until the user has looked at the screen.
  // G3: one persistent notice per blocked period. Only an explicit click
  // opens confirmation; repeated host errors and keystrokes never open dialogs.
  let inputPaused = $state(false)
  function askUnblock() { if (!exited) inputPaused = true }
  function resumeInput() {
    if (exited) return
    if (confirm('部分输入被拒绝或发送结果不明：终端里可能留着半条命令。\n\n确认已检查终端，恢复输入？')) {
      link.unblock(); inputPaused = false
    }
  }

  async function pasteFiles(files: File[]) {
    if (!session) return
    for (const f of files) {
      const isImage = f.type.startsWith('image/')
      const name = f.name || (isImage ? 'clipboard.png' : 'file.bin')
      try {
        progress = `上传 ${name}…`
        toast(`正在上传${isImage ? '图片' : '文件'} ${name}（${Math.round(f.size / 1024)} KB）…`)
        const begin = await post(`/api/sessions/${sid}/paste-file`, { name, size: f.size, image: isImage })
        const hash = await uploadChunks(f, begin, `/api/nodes/${begin.node_id}/fs/uploads/${begin.id}`,
          sent => progress = `上传 ${name}… ${Math.round(sent / f.size * 100)}%`)
        const done = await post(`/api/nodes/${begin.node_id}/fs/uploads/${begin.id}/finish`, { sha256: hash })
        // Either text to type (a path) or keys to send: for Claude Code the
        // node put the image on its clipboard and Alt+V attaches it (M4 第 7 节).
        if (done.keys) {
          link.input(done.keys); toast('图片已发给 Claude Code（应显示为 [Image #1]）')
          // the next image must not replace this one on the clipboard before Claude Code has read it
          if (files.length > 1) await new Promise(r => setTimeout(r, 1200))
        } else {
          term.paste(done.insert ?? done.path + ' ')
          toast(app.profiles.find(p => p.id === session?.profile_id)?.kind === 'claude' && isImage ? '已上传到节点，路径已粘进终端（这台节点的代理没能把图片放进剪贴板，Claude Code 不会把路径当作附件）' : '已上传到节点，路径已粘进终端')
        }
      } catch (e: any) { toast('粘贴失败：' + e.message) }
      finally { progress = '' }
    }
  }
  function onPaste(e: ClipboardEvent) {
    const d = e.clipboardData
    if (!d) return
    // A screenshot tool puts a bitmap on the clipboard: it shows up as a file
    // item (some browsers fill `files`, some only `items`).
    let files = Array.from(d.files ?? [])
    if (!files.length) files = Array.from(d.items ?? []).filter(i => i.kind === 'file').map(i => i.getAsFile()).filter((f): f is File => !!f)
    // Excel or Word put the text and a bitmap of it on the clipboard together: the text is what was meant.
    const text = d.types.includes('text/plain') ? d.getData('text/plain') : ''
    console.debug('paste event', { types: Array.from(d.types), files: files.map(f => f.type + ' ' + f.size), text: text.length })
    if (files.length && !text.trim()) { e.preventDefault(); e.stopImmediatePropagation(); pasteFiles(files) }
    else if (!files.length && !d.types.includes('text/plain')) toast('剪贴板里没有可粘贴的文字或图片（内容类型：' + (Array.from(d.types).join('、') || '空') + '）')
  }
  function onDrop(e: DragEvent) {
    e.preventDefault()
    const files = Array.from(e.dataTransfer?.files ?? [])
    if (files.length) pasteFiles(files)
  }
  async function reopen() {
    if (!session) return
    try {
      const r = await post('/api/sessions', { profile_id: session.profile_id, cwd: session.cwd, cols: term.cols, rows: term.rows })
      await refreshLists()
      replaceTab(sid, r.session.sid)
    } catch (e: any) { toast(e.message) }
  }
  async function attach(e: Event) {
    const files = Array.from((e.target as HTMLInputElement).files ?? [])
    ;(e.target as HTMLInputElement).value = ''
    pasteFiles(files)
  }
</script>

<div class="term-wrap">
  {#if banner || exited || replaying}
    <div class="banner {bannerKind}" data-testid="banner">
      {#if exited}
        会话已结束（{exited.reason}{exited.exit_code !== null ? `，退出码 ${exited.exit_code}` : ''}）
        <button onclick={reopen}>用相同配置重新打开</button>
      {:else if replaying}正在恢复画面…{:else}{banner}{/if}
    </div>
  {/if}
  {#if readOnly || takenBy}
    <div class="banner warn" data-testid="readonly">
      {#if !readOnly}你已接管这个会话，原主人现在只能看；关掉这个标签就交还给原主人。
      {:else if takenBy}{takenBy} 接管了这个会话，你现在只能看。<button onclick={take} data-testid="take">{mine ? '收回' : '接管'}</button>
      {:else}只读：这是别人的会话，只能看，不能输入。<button onclick={take} data-testid="take">接管</button>{/if}
    </div>
  {/if}
  {#if follows && !readOnly}
    <button class="banner info" onclick={() => { follows = false; doFit(); term.focus() }}>尺寸由 {sizeBy} 控制，点击或输入即可接管</button>
  {/if}
  {#if inputPaused && !exited}
    <div class="banner warn" data-testid="input-paused">
      输入已暂停，请先检查终端中是否留有半条命令。
      <button onclick={resumeInput}>已检查，恢复输入</button>
    </div>
  {/if}
  <div class="xterm-host" bind:this={el}>
    {#if awayFromBottom && !sel}
      <button type="button" class="tobottom" title="回到底部" aria-label="回到底部" data-testid="to-bottom" onclick={toBottom}>↓{#if newBelow}<span class="newdot" title="下面有新输出"></span>{/if}</button>
    {/if}
    {#if sel}
      <div class="seltools" data-testid="sel-tools">
        <button type="button" data-testid="sel-copy" onpointerdown={e => { e.preventDefault(); copySelection() }}>复制</button>
        {#if app.mobile}<button type="button" data-testid="sel-tobox" onpointerdown={e => { e.preventDefault(); selectionToBox() }}>发送到输入框</button>{/if}
        {#if selPath}<button type="button" data-testid="sel-download" onpointerdown={e => { e.preventDefault(); selectionDownload() }}>下载</button>{/if}
        <button type="button" onpointerdown={e => { e.preventDefault(); touch?.clear() }}>取消</button>
      </div>
      {#if handles.start}<div class="handle" data-handle="start" data-testid="sel-handle" style:left="{handles.start.x}px" style:top="{handles.start.y}px"></div>{/if}
      {#if handles.end}<div class="handle" data-handle="end" data-testid="sel-handle" style:left="{handles.end.x}px" style:top="{handles.end.y}px"></div>{/if}
    {/if}
  </div>
  <div class="status row">
    <span>{node?.name ?? ''}</span>
    <span class="muted">{cols}×{rows}</span>
    {#if peers > 1}<span class="muted" title="同时打开此会话的设备数">👥 {peers}</span>{/if}
    {#if progress}<span class="muted">{progress}</span>{/if}
    {#if canSend}<button class="send" data-testid="send-other" title="把选中内容粘贴到另一窗格的终端（Ctrl+Shift+S），不经过系统剪贴板，不自动回车" onclick={onsend}>发送到另一窗格 ⇄</button>{/if}
    <label class="attach" title="上传文件并把路径粘贴进终端">📎<input type="file" multiple onchange={attach} /></label>
  </div>
</div>

<style>
  .term-wrap { display: flex; flex-direction: column; height: 100%; }
  .banner { padding: 4px 10px; font-size: 13px; border: none; text-align: left; }
  .banner.info { background: #1f3a5f; }
  .banner.warn { background: #4d3a12; }
  .banner.bad { background: #5a1f1c; }
  .banner button { margin-left: 10px; padding: 1px 8px; }
  .xterm-host { flex: 1; min-height: 0; padding: 4px 0 0 4px; position: relative; touch-action: none; }
  .tobottom { position: absolute; right: 14px; bottom: 12px; z-index: 6; width: 40px; height: 40px; border-radius: 50%; padding: 0; font-size: 20px; line-height: 1;
    background: color-mix(in srgb, var(--panel) 85%, transparent); border: 1px solid var(--line); color: var(--fg); box-shadow: 0 2px 10px rgba(0,0,0,.35); opacity: .9; }
  .tobottom:hover { opacity: 1; }
  .newdot { position: absolute; top: 4px; right: 4px; width: 9px; height: 9px; border-radius: 50%; background: var(--accent); }
  .handle { position: absolute; width: 22px; height: 22px; border-radius: 50%; background: var(--accent); border: 2px solid #fff; transform: translate(-50%, -3px); z-index: 6; box-shadow: 0 1px 4px rgba(0,0,0,.5); }
  .seltools { position: absolute; top: 6px; right: 6px; z-index: 7; display: flex; gap: 6px; padding: 4px; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; box-shadow: 0 4px 16px rgba(0,0,0,.4); }
  .seltools button { padding: 6px 10px; font-size: 14px; user-select: none; -webkit-user-select: none; }
  .status { height: 24px; padding: 0 10px; font-size: 12px; background: var(--panel); border-top: 1px solid var(--line); gap: 14px; }
  .send { padding: 0 8px; font-size: 12px; height: 20px; }
  .attach { margin-left: auto; cursor: pointer; }
  .attach input { display: none; }
</style>

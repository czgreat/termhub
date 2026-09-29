<script lang="ts">
  // Administration (docs/M6 第 10 节, M7 第 3、4 节): users, nodes, CLI profiles, bindings.
  import { get, post, put, patch, del, type User, type Node, type Profile } from '../lib/api'
  import { app, refreshLists, toast } from '../lib/state.svelte'
  import { withReverify } from '../lib/reverify.svelte'

  let { onclose }: { onclose: () => void } = $props()
  let tab = $state<'nodes' | 'profiles' | 'users' | 'prices'>('nodes')
  let users = $state<User[]>([]), nodes = $state<Node[]>([]), profiles = $state<Profile[]>([])
  let templates = $state<any[]>([]), system = $state<any>({})
  let error = $state(''), reveal = $state<{ title: string; lines: string[] } | null>(null)

  async function load() {
    error = ''
    try {
      const [u, n, p, t, s] = await Promise.all([get('/api/admin/users'), get('/api/nodes'), get('/api/profiles'), get('/api/admin/profile-templates'), get('/api/admin/system')])
      users = u.users ?? []; nodes = n.nodes ?? []; profiles = p.profiles ?? []; templates = t.templates ?? []; system = s
    } catch (e: any) { error = e.message }
  }
  $effect(() => { load() })

  async function act(fn: () => Promise<void>) {
    error = ''
    try { await withReverify(fn); await load(); refreshLists() } catch (e: any) { error = e.message }
  }

  // ---- model prices (主人 09-26: 按官方计费折算美元) ----
  // Built-in official prices, and overrides as JSON by model: dollars per
  // million tokens (input, cached, write_5m, write_1h, output, long, window).
  let prices = $state<{ defaults: Record<string, any>; overrides: Record<string, any>; effective: Record<string, any> }>({ defaults: {}, overrides: {}, effective: {} })
  let pricesText = $state('')
  async function loadPrices() {
    try { prices = await get('/api/admin/model-prices'); pricesText = Object.keys(prices.overrides).length ? JSON.stringify(prices.overrides, null, 2) : '' } catch (e: any) { error = e.message }
  }
  $effect(() => { if (tab === 'prices') loadPrices() })
  const effective = $derived(Object.entries(prices.effective ?? {}).sort(([a], [b]) => a.localeCompare(b)))
  const savePrices = () => act(async () => {
    let overrides: Record<string, any> = {}
    if (pricesText.trim()) {
      try { overrides = JSON.parse(pricesText) } catch { throw new Error('不是有效的 JSON') }
    }
    await put('/api/admin/model-prices', { overrides })
    await loadPrices()
    toast('价格已保存，下次刷新用量时生效')
  })
  const money = (v: number | undefined) => v == null ? '–' : '$' + v

  // ---- nodes ----
  let newNode = $state('')
  const createNode = () => act(async () => {
    const r = await post('/api/admin/nodes', { name: newNode })
    newNode = ''
    reveal = { title: `节点 ${r.node.name} 已创建。令牌只显示这一次。`, lines: [
      '在那台机器上，以要运行 agent 的 Windows 用户身份，用 deploy\\install-agent.ps1 安装（登记并设置自启）：',
      `.\\install-agent.ps1 -Hub ${location.origin} -Token ${r.token} -Pin ${system.cert_fingerprint}`,
      '或只登记：termhub-agent enroll --hub … --token … --pin …，然后 termhub-agent run',
    ] }
  })
  const rotate = (n: Node) => act(async () => {
    const r = await post(`/api/admin/nodes/${n.id}/rotate-token`)
    reveal = { title: `${n.name} 的新令牌（旧令牌在新令牌首次使用后失效）`, lines: [`termhub-agent enroll --hub ${location.origin} --token ${r.token} --pin ${system.cert_fingerprint}`] }
  })
  const renameNode = (n: Node) => {
    const name = prompt('节点名称（如 PC1-claude）', n.name)?.trim()
    if (!name || name === n.name) return
    act(async () => { await patch(`/api/admin/nodes/${n.id}`, { name, note: n.note ?? '' }) })
  }
  // Display order: swap with the neighbour, then number every node 1, 2, 3…
  const moveNode = (i: number, d: -1 | 1) => {
    const list = nodes.slice(); const j = i + d
    if (j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j], list[i]]
    act(async () => {
      for (const [k, n] of list.entries()) if (n.position !== k + 1) await patch(`/api/admin/nodes/${n.id}`, { position: k + 1 })
    })
  }
  const nodeAction = (n: Node, a: string) => act(async () => { await post(`/api/admin/nodes/${n.id}/${a}`) })
  const deleteNode = (n: Node) => { if (confirm(`删除节点 ${n.name}？它的 CLI 配置一并删除。`)) act(async () => { await del(`/api/admin/nodes/${n.id}`) }) }

  // ---- profiles ----
  let editing = $state<Profile | null>(null)
  let envText = $state(''), bindings = $state<number[]>([])
  // per bound user: the folders whose history they see, one per line (docs/M10 第 3.4 节)
  let folders = $state<Record<number, string>>({})
  function startProfile(tpl?: any) {
    editing = { id: 0, node_id: nodes[0]?.id ?? 0, name: tpl?.name ?? '', kind: tpl?.kind ?? 'custom', mode: tpl?.mode ?? 'shell',
      shell_path: tpl?.shell_path ?? '', shell_args: [], command: tpl?.command ?? '', args: [], resume_cmd: tpl?.resume_cmd ?? '', continue_cmd: tpl?.continue_cmd ?? '',
      env: {}, default_cwd: '', idle_timeout: 0, quote_style: tpl?.quote_style ?? 'auto', status: 'enabled' }
    envText = tpl?.env_hints?.map((k: string) => k + '=').join('\n') ?? ''
    bindings = []; folders = {}
  }
  async function editProfile(p: Profile) {
    editing = { ...p, args: [...(p.args ?? [])], shell_args: [...(p.shell_args ?? [])] }
    envText = Object.entries(p.env ?? {}).map(([k, v]) => `${k}=${v}`).join('\n')
    try {
      const b = await get(`/api/admin/profiles/${p.id}/bindings`)
      bindings = b.user_ids ?? []
      folders = Object.fromEntries(Object.entries(b.folders ?? {}).map(([k, v]) => [Number(k), (v as string[]).join('\n')]))
    } catch { bindings = []; folders = {} }
  }
  let argsText = $state('')
  $effect(() => { if (editing) argsText = editing.args.join('\n') })
  const saveProfile = () => act(async () => {
    if (!editing) return
    const env: Record<string, string> = {}
    for (const line of envText.split('\n')) {
      const i = line.indexOf('=')
      if (i > 0 && line.slice(i + 1).trim() !== '') env[line.slice(0, i).trim()] = line.slice(i + 1)
    }
    const body = { ...editing, env, args: argsText.split('\n').map(s => s.trim()).filter(Boolean) }
    const r = await post('/api/admin/profiles', body)
    editing.id = r.profile.id // saved: a failing binding below must not make the next save a second profile
    const ranges = Object.fromEntries(bindings.map(u => [u, (folders[u] ?? '').split('\n').map(s => s.trim()).filter(Boolean)]))
    await put(`/api/admin/profiles/${r.profile.id}/bindings`, { user_ids: bindings, folders: ranges })
    editing = null
  })
  const deleteProfile = (p: Profile) => { if (confirm(`删除 CLI 配置 ${p.name}？`)) act(async () => { await del(`/api/admin/profiles/${p.id}`) }) }

  // ---- users ----
  let newUser = $state(''), newRole = $state('user')
  const createUser = () => act(async () => {
    const r = await post('/api/admin/users', { username: newUser, role: newRole })
    reveal = { title: `用户 ${newUser} 已创建。临时密码只显示这一次；首次登录会要求改密码并绑定验证器。`, lines: [r.temp_password] }
    newUser = ''
  })
  const userAction = (u: User, a: string) => act(async () => {
    const r = await post(`/api/admin/users/${u.id}/${a}`)
    if (r.temp_password) reveal = { title: `${u.username} 的临时密码（只显示这一次）`, lines: [r.temp_password] }
  })
  const nodeName = (id: number) => nodes.find(n => n.id === id)?.name ?? String(id)
</script>

<div class="modal-bg">
  <div class="card modal wide">
    <div class="row">
      <h3 style="margin:0">管理</h3>
      <div class="row" style="margin-left:16px">
        <button class:primary={tab === 'nodes'} onclick={() => tab = 'nodes'}>节点</button>
        <button class:primary={tab === 'profiles'} onclick={() => tab = 'profiles'}>CLI 配置</button>
        <button class:primary={tab === 'users'} onclick={() => tab = 'users'}>用户</button>
        <button class:primary={tab === 'prices'} onclick={() => tab = 'prices'}>模型价格</button>
      </div>
      <button style="margin-left:auto" onclick={onclose}>关闭</button>
    </div>
    {#if error}<p class="err">{error}</p>{/if}

    {#if tab === 'nodes'}
      <p class="muted">Hub 证书指纹（登记节点时的 --pin）：<code>{system.cert_fingerprint ?? ''}</code></p>
      <div class="row"><input placeholder="新节点名称，如 工作台-1" bind:value={newNode} data-testid="node-name" /><button class="primary" onclick={createNode} disabled={!newNode} data-testid="node-create">添加节点</button></div>
      <table>
        <thead><tr><th>名称</th><th>状态</th><th>agent</th><th></th></tr></thead>
        <tbody>
        {#each nodes as n, i (n.id)}
          <tr>
            <td>{n.name}{#if n.pwsh_store}<div class="err" style="font-size:12px">pwsh 是商店版，无人登录时无法启动，请装 MSI 版</div>{/if}{#if n.fingerprint_mismatch}<div class="err" style="font-size:12px">机器指纹不符，已拒绝连接</div>{/if}</td>
            <td>{n.online ? '在线' : '离线'}{n.status === 'disabled' ? '（已禁用）' : ''}</td>
            <td class="muted">{n.agent_ver} / {n.host_ver}</td>
            <td class="row" style="flex-wrap:wrap">
              <button onclick={() => rotate(n)}>换令牌</button>
              {#if n.fingerprint_mismatch}<button onclick={() => nodeAction(n, 'clear-fingerprint')}>清除指纹</button>{/if}
              <button onclick={() => nodeAction(n, n.status === 'disabled' ? 'enable' : 'disable')}>{n.status === 'disabled' ? '启用' : '禁用'}</button>
              <button onclick={() => renameNode(n)} data-testid="node-rename">改名</button>
              <button onclick={() => moveNode(i, -1)} disabled={i === 0} title="在列表里上移">↑</button>
              <button onclick={() => moveNode(i, 1)} disabled={i === nodes.length - 1} title="在列表里下移">↓</button>
              <button class="danger" onclick={() => deleteNode(n)}>删除</button>
            </td>
          </tr>
        {/each}
        </tbody>
      </table>

    {:else if tab === 'profiles'}
      {#if editing}
        <div class="card">
          <label for="pn">节点</label>
          <select id="pn" bind:value={editing.node_id} disabled={editing.id !== 0}>{#each nodes as n}<option value={n.id}>{n.name}</option>{/each}</select>
          <label for="pname">显示名</label><input id="pname" bind:value={editing.name} data-testid="profile-name" />
          <div class="row">
            <div style="flex:1"><label for="pm">启动方式</label><select id="pm" bind:value={editing.mode}><option value="shell">经 shell 启动（CLI 退出后回到提示符）</option><option value="direct">直接启动 CLI（退出即结束会话）</option></select></div>
            <div style="flex:1"><label for="pq">粘贴路径的引号</label><select id="pq" bind:value={editing.quote_style}><option value="auto">自动</option><option value="none">不加</option><option value="double">双引号</option><option value="single">单引号</option></select></div>
          </div>
          {#if editing.mode === 'shell'}<label for="ps">shell 路径</label><input id="ps" bind:value={editing.shell_path} />{/if}
          <label for="pc">命令（留空则只开 shell）</label><input id="pc" bind:value={editing.command} data-testid="profile-command" />
          <label for="pa">参数，每行一个</label><textarea id="pa" rows="2" bind:value={argsText}></textarea>
          <label for="pr">恢复历史会话的命令（{'{session}'} 会被替换成会话标识）</label><input id="pr" bind:value={editing.resume_cmd} />
          <label for="pcn">继续上次对话的命令（如 claude --continue；留空则没有“继续上次”）</label><input id="pcn" bind:value={editing.continue_cmd} />
          <label for="pe">环境变量，每行 KEY=value（值只有管理员可见）</label><textarea id="pe" rows="3" bind:value={envText}></textarea>
          <div class="row">
            <div style="flex:1"><label for="pd">默认工作目录</label><input id="pd" bind:value={editing.default_cwd} /></div>
            <div style="width:160px"><label for="pi">空闲超时（秒，0 = 不超时）</label><input id="pi" type="number" min="0" bind:value={editing.idle_timeout} /></div>
          </div>
          <label>绑定给用户（绑定即允许该用户通过网页访问这台机器的全部文件；这不是安全隔离）</label>
          <div class="binds">
            {#each users as u (u.id)}
              <div class="bind">
                <label class="row" style="margin:0"><input type="checkbox" style="width:auto" checked={bindings.includes(u.id)} onchange={e => bindings = (e.target as HTMLInputElement).checked ? [...bindings, u.id] : bindings.filter(x => x !== u.id)} data-testid="bind-{u.username}" /> {u.username}{u.role === 'admin' ? '（管理员，本就有权）' : ''}</label>
                {#if bindings.includes(u.id)}
                  <textarea rows="1" placeholder="历史只显示这些文件夹里的对话，每行一个完整路径；留空 = 全部。第一个也是新建会话的默认目录" bind:value={folders[u.id]} data-testid="bind-folders-{u.username}"></textarea>
                {/if}
              </div>
            {/each}
          </div>
          <div class="row" style="justify-content:flex-end;margin-top:12px"><button onclick={() => editing = null}>取消</button><button class="primary" onclick={saveProfile} data-testid="profile-save">保存</button></div>
        </div>
      {:else}
        <div class="row" style="flex-wrap:wrap">
          <span class="muted">从模板新建：</span>
          {#each templates as t}<button onclick={() => startProfile(t)} data-testid="tpl-{t.kind}">{t.name}</button>{/each}
          <button onclick={() => startProfile()}>空白</button>
        </div>
        <table>
          <thead><tr><th>节点</th><th>名称</th><th>命令</th><th></th></tr></thead>
          <tbody>
          {#each profiles as p (p.id)}
            <tr><td>{nodeName(p.node_id)}</td><td>{p.name}</td><td class="muted">{p.mode === 'direct' ? '' : p.shell_path + ' → '}{p.command} {p.args?.join(' ')}</td>
              <td class="row"><button onclick={() => editProfile(p)}>编辑</button><button class="danger" onclick={() => deleteProfile(p)}>删除</button></td></tr>
          {/each}
          </tbody>
        </table>
      {/if}

    {:else if tab === 'prices'}
      <p class="muted">会话旁显示的美元数按这里的官方 API 价格折算（美元 / 百万 token）。没有价格的模型只显示上下文占用。</p>
      <table>
        <thead><tr><th>模型</th><th>输入</th><th>缓存读</th><th>缓存写 5 分钟</th><th>缓存写 1 小时</th><th>输出</th><th>长上下文（输入/输出）</th></tr></thead>
        <tbody>
        {#each effective as [m, p] (m)}
          <tr class:over={m in prices.overrides}><td><code>{m}</code></td><td>{money(p.input)}</td><td>{money(p.cached)}</td><td>{money(p.write_5m)}</td><td>{money(p.write_1h || p.write_5m)}</td><td>{money(p.output)}</td><td>{p.long ? `${money(p.long.input)} / ${money(p.long.output)}` : ''}</td></tr>
        {/each}
        </tbody>
      </table>
      <p class="muted">要改价格或加模型，在下面按模型写 JSON，每个模型只写要改的字段（其余沿用内置价格），例如 <code>{'{"gpt-7": {"input": 3, "cached": 0.3, "write_5m": 3.75, "output": 15}}'}</code>。留空保存 = 全部恢复内置价格。</p>
      <textarea class="prices" bind:value={pricesText} placeholder="（没有改动）" data-testid="prices-json"></textarea>
      <div class="row" style="justify-content:flex-end"><button class="primary" onclick={savePrices}>保存</button></div>
    {:else}
      <div class="row"><input placeholder="新用户名" bind:value={newUser} /><select bind:value={newRole} style="width:140px"><option value="user">普通用户</option><option value="admin">管理员</option></select><button class="primary" onclick={createUser} disabled={!newUser}>创建</button></div>
      <table>
        <thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>验证器</th><th></th></tr></thead>
        <tbody>
        {#each users as u (u.id)}
          <tr><td>{u.username}</td><td>{u.role}</td><td>{u.status}</td><td>{u.totp_confirmed ? '已绑定' : '未绑定'}</td>
            <td class="row" style="flex-wrap:wrap">
              <button onclick={() => userAction(u, u.status === 'disabled' ? 'enable' : 'disable')}>{u.status === 'disabled' ? '启用' : '禁用'}</button>
              <button onclick={() => userAction(u, 'reset-password')}>重置密码</button>
              <button onclick={() => userAction(u, 'reset-totp')}>重置验证器</button>
              <button onclick={() => userAction(u, 'force-logout')}>强制下线</button>
              <button onclick={() => userAction(u, u.role === 'admin' ? 'make-user' : 'make-admin')}>{u.role === 'admin' ? '降为用户' : '设为管理员'}</button>
            </td></tr>
        {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>

{#if reveal}
  <div class="modal-bg" style="z-index:25">
    <div class="card modal">
      <p>{reveal.title}</p>
      {#each reveal.lines as l}<p><code data-testid="reveal">{l}</code></p>{/each}
      <div class="row" style="justify-content:flex-end"><button class="primary" onclick={() => reveal = null}>已记下</button></div>
    </div>
  </div>
{/if}

<style>
  .wide { width: min(960px, 96vw); }
  .prices { width: 100%; min-height: 120px; font: 12px ui-monospace, Consolas, monospace; }
  tr.over td { color: var(--warn); }
  h3 { margin-bottom: 8px; }
  .binds { display: flex; flex-direction: column; gap: 6px; }
  .bind textarea { margin-top: 4px; font-size: 12px; min-height: 32px; resize: vertical; }
</style>

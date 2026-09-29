<script lang="ts">
  // Quick-start guide (使用帮助): opens by itself the first time a user signs
  // in on a device, can be skipped, and is always there again under 帮助 (电脑：
  // 右上角 ？ 或侧栏“帮助”；手机：右上角 ⋮ → 使用帮助). Two tabs, 电脑 and 手机,
  // starting on the one the user is on.
  import { app } from '../lib/state.svelte'

  let { onclose }: { onclose: () => void } = $props()
  let tab = $state<'pc' | 'phone'>(app.mobile ? 'phone' : 'pc')
</script>

<div class="modal-bg" role="presentation" onclick={onclose}>
  <div class="card guide" role="dialog" aria-modal="true" aria-label="使用帮助" data-testid="guide" onclick={e => e.stopPropagation()}>
    <div class="head">
      <h3>使用帮助</h3>
      <div class="tabs">
        <button class:primary={tab === 'pc'} onclick={() => tab = 'pc'} data-testid="guide-pc">电脑</button>
        <button class:primary={tab === 'phone'} onclick={() => tab = 'phone'} data-testid="guide-phone">手机</button>
      </div>
    </div>

    <div class="body">
      <section>
        <h4>三步开始</h4>
        <ol class="steps">
          {#if tab === 'pc'}
            <li><b>新建会话</b>：左侧“＋ 新建会话”，选“CLI 配置”（显示为“机器 · 配置名”，如 <code>PC1-claude · Claude Code</code>）和工作目录（“🖥 桌面”一点就选好），点启动。</li>
            <li><b>直接在终端里打字</b>，和在那台电脑上用 Claude Code / Codex 一样。</li>
            <li><b>程序问你选什么时</b>，终端底部会出现选项卡片，点一下就选好。</li>
          {:else}
            <li><b>新建会话</b>：左上 ☰ → “＋ 新建会话”，选“CLI 配置”和工作目录（“🖥 桌面”一点就选好），点启动。</li>
            <li><b>在底部输入框写好，点“发送”</b>：整段送进终端并回车。</li>
            <li><b>程序问你选什么时</b>，屏幕下方出现选项卡片，点一下就选好。</li>
          {/if}
        </ol>
      </section>

      <section>
        <h4>常用操作</h4>
        {#if tab === 'pc'}
          <dl>
            <dt>标签</dt><dd>每个会话一个标签。标签上的 × 只是关掉标签，会话在那台电脑上继续跑；左侧“会话”栏点它就能接着用。</dd>
            <dt>状态点</dt><dd>标签和左侧“会话”栏的名字前面：灰点闪烁是 Claude Code 正在干活，<b>绿点</b>是做完了你还没看（点开就消失），<b>橙点</b>是它在问你选项、等你确认。浏览器标签页标题上的 (2) 表示有两个在等你。</dd>
            <dt>结束会话</dt><dd>左侧<b>“会话”栏</b>里鼠标移到那一行，点右边的 ■。“项目”栏是历史记录，文件夹旁的绿点只表示那里有会话在跑，结束要回“会话”栏。</dd>
            <dt>分屏</dt><dd>右上角 ◫ 左右分屏、⊟ 上下分屏、▣ 取消分屏；拖中间的分隔条调大小。鼠标停在按钮上会显示说明。</dd>
            <dt>复制粘贴</dt><dd>选中文字后 Ctrl+C 复制（没选中时 Ctrl+C 是中断）；Ctrl+V 粘贴。粘贴或拖入图片、文件，会上传到那台电脑，图片直接交给 Claude Code。</dd>
            <dt>下载</dt><dd>点终端里显示的文件路径就能下载；文件夹会打包成 zip。</dd>
            <dt>选项卡片</dt><dd>单选点一下即可；多选题先勾选，再点“提交”。卡片上还有 Esc（取消）、Tab（补充说明）等按钮。</dd>
            <dt>断线不怕</dt><dd>关掉浏览器、断网、换电脑都不影响会话，重新打开后从左侧列表点开，会接上之前的画面。</dd>
          </dl>
          <h4>快捷键</h4>
          <table class="keys">
            <tbody>
              <tr><td><kbd>Ctrl+Shift+U</kbd></td><td>新建会话</td></tr>
              <tr><td><kbd>Ctrl+Shift+D</kbd></td><td>分屏 / 取消分屏</td></tr>
              <tr><td><kbd>Ctrl+Shift+O</kbd></td><td>在两个窗格之间切换</td></tr>
              <tr><td><kbd>Ctrl+Shift+H</kbd> / <kbd>L</kbd></td><td>上一个 / 下一个标签</td></tr>
              <tr><td><kbd>Ctrl+Shift+S</kbd></td><td>把选中的文字发到另一个窗格（不回车）</td></tr>
              <tr><td><kbd>Ctrl+Shift+C</kbd></td><td>复制选中的文字</td></tr>
            </tbody>
          </table>
        {:else}
          <dl>
            <dt>发送按钮</dt><dd>点一下：整段发送并回车；输入框空着点：只按一下回车；<b>长按</b>：先按 Esc 打断正在做的事，再发送。</dd>
            <dt>输入方式</dt><dd>右上角 ▤ / ⌨ 切换：“输入框方式”（默认，适合中文和长段文字）或“直接在终端里打字”。</dd>
            <dt>按键栏</dt><dd>最下面一排：Esc、Tab、Ctrl、方向键（按住连发）、^C 中断、⏎ 回车、粘贴、图片（发给 Claude Code）。左右滑动还有 Home、End、翻页等。</dd>
            <dt>看历史、复制</dt><dd>在终端上下滑动看之前的输出；长按文字可以选中，拖两端调整范围，再点“复制”或“发送到输入框”。</dd>
            <dt>切换、结束</dt><dd>点顶部标题切换已打开的会话；右上 ⋮ 里有新建、<b>结束会话</b>、关闭标签、退出。从屏幕左边缘向右滑也能打开会话列表。侧栏“项目”里是历史记录，文件夹旁的绿点只表示那里有会话在跑，不能在那里结束。</dd>
            <dt>状态点</dt><dd>顶部标题和会话列表的名字前面：灰点闪烁是正在干活，<b>绿点</b>是做完了还没看，<b>橙点</b>是在问你选项。标题右边 ▾ 后面的点表示别的会话在等你。</dd>
            <dt>下载</dt><dd>长按终端里的文件路径选中，再点“下载”。</dd>
            <dt>断网、切后台</dt><dd>用输入框时，连不上会提示暂时发不出去，内容留在输入框里，接上后再点发送；直接在终端里打字时，按键会排队，一分钟内接上就按顺序补发。没发的草稿存在这台手机上。</dd>
            <dt>装到桌面</dt><dd>浏览器菜单里“添加到主屏幕”（iPhone：Safari 分享按钮 → 添加到主屏幕），以后像 App 一样打开。</dd>
          </dl>
        {/if}
      </section>

      <section>
        <h4>会话与历史</h4>
        <dl>
          <dt>花费与上下文</dt><dd>Claude Code 和 Codex 的会话旁显示如 <code>$3.42 · 38%</code>：这段对话按官方 API 价格折算的美元数（订阅用户实际不按这个付费，只作参考），和上下文窗口已用的比例。鼠标停在标签上看模型等细节；没有官方价格的模型（如 DeepSeek）只显示上下文。价格表在“管理 → 模型价格”。</dd>
          <dt>历史对话</dt><dd>侧栏“项目”按机器和文件夹列出以前的对话。点一段先只读看记录，要接着聊点“继续这个对话”。新建会话时也可以选“新对话”“继续上次”或用 CLI 自带界面挑一段。</dd>
          <dt>隐藏、改名</dt><dd>文件夹和对话的“⋯”菜单都能隐藏，文件夹还能重命名。只改网页上的显示，文件和对话都不删；隐藏的在“已隐藏”里能恢复。</dd>
          <dt>一个文件夹一个</dt><dd>同一台机器的同一个文件夹，Claude Code 和 Codex 各只能开一个会话。你自己开着的会直接切过去；别人开着的会提示被占用。</dd>
          <dt>别人的会话</dt><dd>管理员打开别人的会话默认只能看。点“接管”要再验证一次，原主人可以收回；接管的人关掉标签就交还。</dd>
          <dt>输入已暂停</dt><dd>连接出过问题时终端上方会出现这条提示：先看终端里有没有留下半条命令，再点“已检查，恢复输入”。</dd>
          <dt>草稿与保留副本</dt><dd>没发出去的草稿，以及发送结果不确定时留下的副本，都列在侧栏这一栏，会话结束或标签关掉后也在。草稿可以删除，副本可以放回草稿或清除。</dd>
          <dt>网页和本机终端</dt><dd>网页里开的会话就跑在那台电脑上，和在它桌面开终端是同一个程序。同一段对话不要两边同时开：换到本机前先在网页结束会话，再在那个文件夹运行 <code>claude --continue</code>（Codex 是 <code>codex resume --last</code>）；换回网页用“继续上次”。本机开的会话不在“会话”栏里，但对话会出现在“项目”里（在你能看的文件夹范围内时）。</dd>
        </dl>
      </section>
    </div>

    <div class="foot">
      <span class="muted">以后可以在{app.mobile ? '右上角 ⋮ →“使用帮助”' : '右上角或左下角的 ？'}再打开。</span>
      <button class="primary" onclick={onclose} data-testid="guide-close">知道了</button>
    </div>
  </div>
</div>

<style>
  .modal-bg { position: fixed; inset: 0; background: rgba(0,0,0,.55); z-index: 40; display: flex; align-items: center; justify-content: center; padding: 16px; }
  .guide { width: min(640px, 100%); max-height: calc(var(--vvh, 100vh) - 32px); display: flex; flex-direction: column; padding: 0; overflow: hidden; }
  .head { display: flex; align-items: center; gap: 12px; padding: 14px 16px 10px; border-bottom: 1px solid var(--line); }
  .head h3 { margin: 0; flex: 1; }
  .tabs { display: flex; gap: 6px; }
  .body { overflow-y: auto; padding: 4px 16px 12px; }
  h4 { margin: 14px 0 6px; font-size: 14px; color: var(--accent); }
  .steps { margin: 0; padding-left: 20px; display: grid; gap: 6px; }
  dl { margin: 0; display: grid; grid-template-columns: max-content 1fr; gap: 6px 12px; }
  dt { color: var(--fg); font-weight: 600; white-space: nowrap; }
  dd { margin: 0; color: var(--dim); }
  .keys td { border: none; padding: 3px 8px 3px 0; color: var(--dim); }
  kbd { font: 12px ui-monospace, Consolas, monospace; padding: 1px 6px; border: 1px solid var(--line); border-bottom-width: 2px; border-radius: 4px; background: var(--panel-2); color: var(--fg); }
  code { font-size: 12px; }
  .foot { display: flex; align-items: center; gap: 12px; padding: 10px 16px calc(10px + env(safe-area-inset-bottom)); border-top: 1px solid var(--line); }
  .foot .muted { flex: 1; font-size: 12px; }
  @media (max-width: 520px) {
    dl { grid-template-columns: 1fr; gap: 2px; }
    dd { margin-bottom: 8px; }
  }
</style>

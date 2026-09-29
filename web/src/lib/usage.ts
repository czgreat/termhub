// The dollar figure and context share shown for an AI session (主人 2026-09-26:
// 按官方计费折算美元、上下文占用; no token counts). The Hub prices what the node
// reads from the CLI's own history file (internal/hub/route/usage.go).

export type Usage = { cost: number | null; context_pct: number | null; model?: string; conv?: string; unpriced?: string[]; updated?: number }

export function fmtCost(c: number | null | undefined): string {
  if (c == null) return ''
  if (c < 0.01) return '<$0.01'
  if (c < 100) return '$' + c.toFixed(2)
  return '$' + Math.round(c).toLocaleString('en-US')
}

/** "$3.42 · 38%": whichever of the two is known. */
export function usageText(u: Usage | undefined): string {
  if (!u) return ''
  const parts: string[] = []
  const c = fmtCost(u.cost)
  if (c) parts.push(c)
  if (u.context_pct != null) parts.push(Math.round(u.context_pct) + '%')
  return parts.join(' · ')
}

export function usageTip(u: Usage | undefined): string {
  if (!u) return ''
  const lines: string[] = []
  if (u.cost != null) lines.push(`本对话按官方 API 价格折算：${fmtCost(u.cost)}`)
  if (u.context_pct != null) lines.push(`上下文占用：${u.context_pct}%`)
  if (u.model) lines.push(`模型：${u.model}`)
  if (u.unpriced?.length) lines.push(`没有价格、未计入：${u.unpriced.join('、')}`)
  return lines.join('\n')
}

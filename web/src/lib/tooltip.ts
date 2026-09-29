// Tooltips for mouse users (电脑端): every element with a title gets a quick,
// readable hint instead of the browser's slow native one. While the pointer is
// on the element its title is parked in data-tip, so the native hint never
// doubles it, and put back when the pointer leaves. Touch screens are
// left alone (no hover there; the guide explains the buttons instead).
export function initTooltips() {
  if (!matchMedia('(hover: hover) and (pointer: fine)').matches) return
  const tip = document.createElement('div')
  tip.className = 'tip'
  tip.setAttribute('role', 'tooltip')
  document.body.appendChild(tip)
  let timer: number | undefined, current: HTMLElement | null = null

  const hide = () => {
    clearTimeout(timer); tip.classList.remove('show')
    if (current?.dataset.tip && !current.hasAttribute('title')) current.setAttribute('title', current.dataset.tip)
    current = null
  }
  const show = (el: HTMLElement) => {
    const text = el.dataset.tip
    if (!text || !el.isConnected) return
    tip.textContent = text
    tip.classList.add('show')
    const r = el.getBoundingClientRect(), t = tip.getBoundingClientRect()
    let top = r.bottom + 6
    if (top + t.height > innerHeight - 4) top = r.top - t.height - 6
    const left = Math.min(Math.max(4, r.left + r.width / 2 - t.width / 2), innerWidth - t.width - 4)
    tip.style.top = `${Math.max(4, top)}px`
    tip.style.left = `${left}px`
  }
  document.addEventListener('mouseover', e => {
    const el = (e.target as Element | null)?.closest?.('[title],[data-tip]') as HTMLElement | null
    if (el === current) return
    hide()
    if (!el) return
    const title = el.getAttribute('title')
    if (title) {
      el.dataset.tip = title
      el.removeAttribute('title')
      if (!el.getAttribute('aria-label') && !el.textContent?.trim()) el.setAttribute('aria-label', title)
    }
    current = el
    timer = window.setTimeout(() => show(el), 350)
  })
  document.addEventListener('mousedown', hide, true)
  document.addEventListener('scroll', hide, true)
  window.addEventListener('blur', hide)
}

import { onMount } from 'svelte'
export const egressClock = $state({ now: Date.now() })
let users = 0, timer: ReturnType<typeof setInterval> | undefined
// One local UI clock, no network requests and no new public-IP probes.
export function useEgressClock() {
  onMount(() => {
    if (users++ === 0) { egressClock.now = Date.now(); timer = setInterval(() => egressClock.now = Date.now(), 1000) }
    return () => { if (--users === 0) clearInterval(timer) }
  })
}

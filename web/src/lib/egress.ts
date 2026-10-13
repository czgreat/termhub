import type { Egress, Node } from './api'
export function egressBlocked(status: Egress | undefined, now = Date.now()) {
  return !!status && (!status.allowed || now >= status.valid_until * 1000)
}

export function mergeEgress(incoming: Node[], current: Node[], events: Map<number, number>, asked: number) {
  return incoming.map(node => (events.get(node.id) ?? 0) > asked ? { ...node, egress: current.find(old => old.id === node.id)?.egress ?? node.egress } : node)
}

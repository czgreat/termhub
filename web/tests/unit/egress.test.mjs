import { test } from 'node:test'
import assert from 'node:assert/strict'
import { egressBlocked, mergeEgress } from '../../src/lib/egress.ts'

test('node badges and start actions share an expiry decision', () => {
  assert.equal(egressBlocked(undefined, 1000), false)
  assert.equal(egressBlocked({ allowed: true, valid_until: 1 }, 999), false)
  assert.equal(egressBlocked({ allowed: true, valid_until: 1 }, 1000), true)
  assert.equal(egressBlocked({ allowed: false, valid_until: 100 }, 1000), true)
})
test('a slow node-list response cannot overwrite a newer egress event', () => {
  const old = [{ id: 1, egress: { allowed: true, epoch: 4 } }]
  const current = [{ id: 1, egress: { allowed: false, epoch: 5 } }]
  const events = new Map([[1, 2]])
  assert.equal(mergeEgress(old, current, events, 1)[0].egress.allowed, false)
  // A newly started request may accept the new Hub boot's different epoch seed.
  assert.equal(mergeEgress(old, current, events, 2)[0].egress.allowed, true)
})

test('an event arriving before the initial list does not erase protection', () => {
  const response = [{ id: 1, egress: { allowed: false, epoch: 2 } }]
  assert.equal(mergeEgress(response, [], new Map([[1, 2]]), 1)[0].egress.allowed, false)
})

// Linux node paths in the browser (2026-10-11).
// Run: node --import ./tests/unit/register.mjs --test "tests/unit/*.test.mjs"
import { test } from 'node:test'
import assert from 'node:assert/strict'

const P = await import('../../src/lib/nodepath.ts')

test('posix clean, base names, joins and crumbs', () => {
  assert.equal(P.posixClean('/home//Alice/./work/../work/'), '/home/Alice/work')
  assert.equal(P.posixClean('/../..'), '/')
  assert.equal(P.isPosix('/home/a'), true)
  assert.equal(P.isPosix('//server/share'), false)
  assert.equal(P.isPosix('C:\\Users'), false)
  assert.equal(P.baseName('/home/Alice/claudeproject/'), 'claudeproject')
  assert.equal(P.baseName('C:\\Users\\me\\proj'), 'proj')
  assert.equal(P.joinPath('/home/u/', 'x'), '/home/u/x')
  assert.equal(P.joinPath('/', 'home'), '/home')
  assert.equal(P.joinPath('C:\\', 'Users'), 'C:\\Users')
  assert.deepEqual(P.crumbsOf('/home/u'), [{ name: '/', path: '/' }, { name: 'home', path: '/home' }, { name: 'u', path: '/home/u' }])
  assert.deepEqual(P.crumbsOf('C:\\Users\\me').map(c => c.path), ['C:\\', 'C:\\Users', 'C:\\Users\\me'])
})

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { sessionInput } from '../input-wire.ts'

test('phone wire assertions collect acknowledged Esc, Unicode paste, Enter and binary keys', () => {
 const inputs=['\x1b','echo 打断后发送','\r']
 const frames=inputs.map((s,i)=>JSON.stringify({t:'input_batch',id:i+1,data:Buffer.from(s).toString('base64')}))
 frames.unshift(JSON.stringify({t:'resize',cols:80,rows:24}))
 frames.push(Buffer.from('\x1b[A'))
 const text=frames.map(sessionInput).filter(Boolean).map(b=>b.toString('utf8')).join('')
 assert.equal(text,'\x1becho 打断后发送\r\x1b[A')
 assert.equal(sessionInput('not JSON'),null)
 assert.equal(sessionInput(JSON.stringify({t:'input_result',re:1})),null)
})

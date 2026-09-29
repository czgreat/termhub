// Decode both browser input transports for wire assertions. Other control
// messages are not terminal input (review #7 F09 added acknowledged chunks).
export function sessionInput(payload: string | Buffer): Buffer | null {
  if (typeof payload !== 'string') return payload
  try {
    const m = JSON.parse(payload)
    return m.t === 'input_batch' && typeof m.data === 'string' ? Buffer.from(m.data, 'base64') : null
  } catch { return null }
}

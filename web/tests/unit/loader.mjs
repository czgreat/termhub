// Lets node:test import the page's own modules: TypeScript is stripped, a
// .svelte.ts module (runes) is compiled by Svelte, and "./api" finds "./api.ts"
// as Vite would. Registered by register.mjs.
import { stripTypeScriptTypes } from 'node:module'
import { existsSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { compile, compileModule } from 'svelte/compiler'

export async function resolve(spec, ctx, next) {
  if (spec.startsWith('.') && ctx.parentURL?.includes('/src/') && !/\.[cm]?[jt]s$/.test(spec)) {
    for (const ext of ['.ts', '.svelte.ts', '.js']) {
      const url = new URL(spec + ext, ctx.parentURL)
      if (existsSync(fileURLToPath(url))) return { url: url.href, shortCircuit: true }
    }
  }
  return next(spec, ctx)
}

export async function load(url, ctx, next) {
  if (url.startsWith('file:') && url.endsWith('.svelte')) {
    const file = fileURLToPath(url)
    return { format: 'module', source: compile(await readFile(file, 'utf8'), { filename: file, generate: 'server', dev: false }).js.code, shortCircuit: true }
  }
  if (!url.startsWith('file:') || !url.endsWith('.ts')) return next(url, ctx)
  const file = fileURLToPath(url)
  let code = stripTypeScriptTypes(await readFile(file, 'utf8'), { mode: 'transform' })
  if (file.endsWith('.svelte.ts')) code = compileModule(code, { filename: file, generate: 'client', dev: false }).js.code
  return { format: 'module', source: code, shortCircuit: true }
}

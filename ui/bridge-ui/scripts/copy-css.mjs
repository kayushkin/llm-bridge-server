// tsc emits no CSS. Every stylesheet a component imports relatively must sit in
// dist/ at the same path it has in src/, or the consumer's bundler resolves the
// import against a file that is not there.
import { cpSync, mkdirSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) walk(p, out)
    else if (name.endsWith('.css')) out.push(p)
  }
  return out
}
for (const src of walk('src')) {
  const dest = join('dist', relative('src', src))
  mkdirSync(dirname(dest), { recursive: true })
  cpSync(src, dest)
}

// Remove from styles.css every rule whose classes nothing live references.
//
//   node scripts/prune-orphaned-css.mjs          # report only
//   node scripts/prune-orphaned-css.mjs --write  # rewrite styles.css
//
// "Live" is every source that can put a class name on screen or assert on one:
// this package's src/, scripts/ and test/, dash's src/ and e2e/, and chat-core's
// src/. A class is referenced if its exact name appears in any of them, OR if it
// begins with a prefix some template literal builds dynamically — the twenty
// `bc-foo-${state}` sites are why a plain substring sweep would break styling
// silently. A rule with several selectors keeps the ones that survive; a rule
// with none is removed with the comment that introduced it.
import postcss from 'postcss'
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { homedir } from 'node:os'

const ROOTS = [
  'src', 'scripts', 'test',
  join(homedir(), 'repos/dash/src'), join(homedir(), 'repos/dash/e2e'),
  join(homedir(), 'repos/chat-core/src'),
]
function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (name === 'node_modules' || name === 'dist') continue
    if (statSync(p).isDirectory()) walk(p, out)
    else if (/\.(tsx?|mjs|css|html)$/.test(name)) out.push(p)
  }
  return out
}
let corpus = ''
for (const r of ROOTS) for (const f of walk(r)) corpus += readFileSync(f, 'utf8') + '\n'

// Dynamic prefixes: `bc-foo-${…}` inside a template literal, or 'bc-foo-' + …
const dynamicPrefixes = new Set()
for (const m of corpus.matchAll(/([A-Za-z][\w-]*-)\$\{/g)) dynamicPrefixes.add(m[1])
for (const m of corpus.matchAll(/'([A-Za-z][\w-]*-)'\s*\+/g)) dynamicPrefixes.add(m[1])
const referenced = (cls) => {
  if (new RegExp('(?<![\\w-])' + cls.replace(/[-]/g, '\\-') + '(?![\\w-])').test(corpus)) return true
  for (const p of dynamicPrefixes) if (cls.startsWith(p)) return true
  return false
}

const css = readFileSync('styles.css', 'utf8')
const root = postcss.parse(css)
let removedRules = 0, trimmedRules = 0, removedLines = 0
const orphaned = new Set()
root.walkRules((rule) => {
  if (rule.parent?.type === 'atrule' && /keyframes/.test(rule.parent.name)) return
  const selectors = rule.selectors
  const keep = selectors.filter((sel) => {
    const classes = [...sel.matchAll(/\.(-?[_A-Za-z][\w-]*)/g)].map((m) => m[1])
    if (classes.length === 0) return true
    const alive = classes.some(referenced)
    if (!alive) for (const c of classes) orphaned.add(c)
    return alive
  })
  if (keep.length === selectors.length) return
  if (keep.length === 0) {
    removedLines += rule.toString().split('\n').length
    const prev = rule.prev()
    if (prev?.type === 'comment') { removedLines += prev.toString().split('\n').length; prev.remove() }
    rule.remove(); removedRules++
  } else {
    rule.selectors = keep; trimmedRules++
  }
})
root.walkAtRules((at) => { if (/media|supports/.test(at.name) && at.nodes?.length === 0) at.remove() })

console.log(`dynamic prefixes honored: ${[...dynamicPrefixes].filter((p) => p.startsWith('b')).length}`)
console.log(`orphaned classes: ${orphaned.size}`)
console.log(`rules removed: ${removedRules}, selectors trimmed from rules: ${trimmedRules}, ~lines removed: ${removedLines}`)
if (process.argv.includes('--write')) {
  writeFileSync('styles.css', root.toString().replace(/\n{3,}/g, '\n\n'))
  console.log('styles.css rewritten')
} else {
  console.log('sample:', [...orphaned].slice(0, 30).join(' '))
}

// Live browser canary for the sticky-bottom pin across a session switch.
//
//   node scripts/sticky-bottom-canary.mjs                      # against the local
//                                                              # standalone dev server
//   BASE=https://dash.kayushkin.com DASH_TOKEN=$(...) \
//     node scripts/sticky-bottom-canary.mjs [<session_id>] [<first_sidebar_index>]
//
// The sidebar index is where to START looking for a second session, not which
// one to use: the list is newest-first and this host mints sessions all day,
// so the script walks forward until it finds one that actually overflows a
// pane. A session that fits in the pane cannot show this bug.
//
// The panes are not remounted when the chat switches session — the rows are
// replaced under them — so everything about where a newly opened session
// starts is a question about state a hook carried over from the log before
// it. `npm run check` renders through react-dom/server, which has no layout
// and no scrollTop, so it cannot see any of this. Only a browser can.
//
// The load-bearing sequence is the one that reproduced the bug by hand:
// open a long session, scroll to the TOP of every pane, then pick a different
// session from the sidebar. Every pane that overflows must open at its
// newest end. Before the fix, Thread opened 971px from the bottom.
//
// It also checks the other direction, which is the way a careless fix breaks
// things: filtering the log the user is currently reading must NOT drag them
// to the bottom. A pin keyed on "the rows changed" passes the first check and
// fails this one.
//
// Not wired into `npm run check`: it needs a running app and Playwright. It
// borrows dash's Playwright install rather than adding one here — bridge-ui
// is a library and should not carry a browser.
//
// ⚠ Selector traps, all of which cost a run to find:
//   - Only the Turns pane is on by default. The other two are behind
//     `.bc-pane-toggle[aria-label="Timeline"|"Thread"]`.
//   - The scrolling element of each pane is `.bc-turns-body`, `.bc-thread`
//     (inside `.bc-thread-wrap`) and `.bc-timeline-body`. The wrappers do
//     not scroll.
//   - `.bc-session-item-main` is also what the sidebar's search results use;
//     index into it only after the list has settled.
//   - The first child of a pane is a position:sticky filter bar, on screen at
//     every scroll position, and a single row can be taller than the pane. Any
//     check that picks a node to watch has to survive both.
import { chromium } from '/home/kayushkincom/repos/dash/node_modules/playwright/index.mjs'

const BASE = process.env.BASE || 'http://127.0.0.1:5199'
const TOKEN = process.env.DASH_TOKEN
const SESSION = process.argv[2] || 'br_1785171126409277953'
const SIDEBAR_INDEX = Number(process.argv[3] ?? 3)

// The hook's own threshold: within this many pixels of the bottom counts as
// at the bottom. Asserting on the same number the code uses, not a rounder
// one, keeps the check honest about what it is measuring.
const THRESHOLD = 40

let failures = 0
const check = (name, cond, detail) => {
  if (cond) console.log(`  ok   ${name}${detail ? ` — ${detail}` : ''}`)
  else { console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ''}`); failures++ }
}

const PANES = {
  turns: '.bc-turns-body',
  thread: '.bc-thread',
  timeline: '.bc-timeline-body',
}

const browser = await chromium.launch({ headless: true })
const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } })
if (TOKEN) {
  const domain = new URL(BASE).hostname
  await ctx.addCookies([{ name: 'dash_token', value: TOKEN, domain, path: '/' }])
}
const page = await ctx.newPage()
page.on('console', m => { if (m.type() === 'error') console.log(`  [console.error] ${m.text().slice(0, 200)}`) })
page.on('pageerror', e => console.log(`  [pageerror] ${String(e).slice(0, 200)}`))

/** Distance from the bottom of every pane, plus how much each can scroll. */
const measure = () => page.evaluate(panes => {
  const out = {}
  for (const [name, selector] of Object.entries(panes)) {
    const el = document.querySelector(selector)
    out[name] = el
      ? {
          present: true,
          distance: Math.round(el.scrollHeight - el.scrollTop - el.clientHeight),
          overflow: el.scrollHeight - el.clientHeight,
          scrollTop: Math.round(el.scrollTop),
        }
      : { present: false }
  }
  return out
}, PANES)

const scrollAllToTop = () => page.evaluate(panes => {
  for (const selector of Object.values(panes)) {
    const el = document.querySelector(selector)
    if (el) el.scrollTop = 0
  }
}, PANES)

const describe = (name, m) => `${name} ${m.present ? `${m.distance}px from the bottom of ${m.overflow}px` : 'absent'}`

console.log(`opening ${BASE}/?session=${SESSION}`)
await page.goto(`${BASE}/?session=${SESSION}`, { waitUntil: 'domcontentloaded', timeout: 120000 })
await page.waitForSelector('.bc-turns-item', { timeout: 300000 })
await page.waitForTimeout(8000)

// --- 1. a session opened from cold starts at its newest end ------------------
// Only the Turns pane is on by default, so this is the one pane there is to
// measure here. The other two are measured after they are shown, which is a
// different event and is checked separately below.
console.log('\n1. the long session, opened cold')
{
  const m = (await measure()).turns
  check('turns is at the bottom', m.present && m.distance <= THRESHOLD, describe('turns', m))
}

// --- 1b. showing another pane must not cost the pin -------------------------
// Showing a pane reflows the ones already open narrower and therefore taller,
// and the browser fires a scroll event for the growth with `scrollTop`
// unchanged. Read as the reader scrolling away, that event drops stickiness
// and the pin never comes back: measured at 416px after the first toggle and
// 969px after the second, on a pane the reader had not touched.
console.log('\n1b. showing the Timeline and Thread panes')
await page.locator('.bc-pane-toggle[aria-label="Timeline"]').click({ timeout: 30000 })
await page.waitForTimeout(3000)
{
  const m = (await measure()).turns
  check('turns held its pin while the Timeline pane opened',
    m.present && m.distance <= THRESHOLD, describe('turns', m))
}
await page.locator('.bc-pane-toggle[aria-label="Thread"]').click({ timeout: 30000 })
await page.waitForSelector('.bc-timeline-body .bc-tl-item', { timeout: 120000 })
await page.waitForTimeout(6000)
const cold = await measure()
for (const [name, m] of Object.entries(cold)) {
  check(`${name} is at the bottom once every pane is shown`,
    m.present && m.distance <= THRESHOLD, describe(name, m))
}

// --- 2b. growth must not move a reader who is scrolled up -------------------
// The pin ignores stickiness-dropping scroll events that came from the content
// growing. The danger in that is the mirror image of the bug it fixes: if it
// also ignored the direction, a reader scrolled up would be dragged to the
// bottom by every event that arrives. So grow the content ABOVE them — which
// is what fires the anchoring scroll event — and watch one live node.
console.log('\n2b. content growing above a scrolled-up reader must leave them alone')
{
  const grown = await page.evaluate(async selector => {
    const el = document.querySelector(selector)
    el.scrollTop = Math.floor((el.scrollHeight - el.clientHeight) / 2)
    await new Promise(r => setTimeout(r, 600))
    // The pane's first child is a position:sticky filter bar that is on screen
    // at every scroll position. Growing the very element being tracked would
    // anchor its own top by construction and pass whatever the hook did, so
    // the grown element and the watched element are kept apart, and neither
    // is allowed to be sticky.
    const items = [...el.children].filter(c => getComputedStyle(c).position !== 'sticky')
    const rect = el.getBoundingClientRect()
    // Grow something that is entirely ABOVE the viewport: that is the case
    // the browser anchors, and the anchoring is what fires the scroll event
    // this fix has to read correctly. Growth lower down just pushes the rest
    // of the list down and says nothing about the hook.
    const grower = [...items].reverse().find(c => c.getBoundingClientRect().bottom < rect.top) || items[0]
    // A single row in this pane can be taller than the pane itself, so
    // "fully on screen" and even "its top edge is on screen" can both find
    // nothing. What the browser anchors to is the first element that reaches
    // into the viewport at all, so that is the element to watch.
    const node = items.find(c => c !== grower && c.getBoundingClientRect().bottom > rect.top)
      || items.find(c => c !== grower)
    if (!node) return { skipped: 'the pane has only one non-sticky child' }
    const growerWasAboveTheViewport = grower.getBoundingClientRect().bottom < rect.top
    const beforeTop = node.getBoundingClientRect().top
    const beforeScrollHeight = el.scrollHeight

    // Grow an existing child rather than inserting one: React reconciles a
    // foreign child straight back out again, and the growth has to survive
    // long enough to be measured.
    const restore = grower.style.minHeight
    grower.style.minHeight = `${grower.getBoundingClientRect().height + 3000}px`
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))
    await new Promise(r => setTimeout(r, 800))

    const afterTop = node.getBoundingClientRect().top
    const insertedPx = el.scrollHeight - beforeScrollHeight
    const distance = Math.round(el.scrollHeight - el.scrollTop - el.clientHeight)
    grower.style.minHeight = restore
    return {
      beforeTop, afterTop, insertedPx, distance, growerWasAboveTheViewport,
      moved: Math.abs(afterTop - beforeTop),
      children: items.length, growerIndex: items.indexOf(grower), nodeIndex: items.indexOf(node),
    }
  }, PANES.turns)
  if (grown.skipped) {
    console.log(`  skip — ${grown.skipped}`)
  } else {
    console.log(`  (${grown.children} children, grew #${grown.growerIndex}, watched #${grown.nodeIndex}, above=${grown.growerWasAboveTheViewport})`)
    check('content really did grow', grown.insertedPx > 1000, `${grown.insertedPx}px`)
    // The claim that matters: growth does not decide where the reader is.
    check('the reader was not dragged to the bottom by it',
      grown.distance > THRESHOLD, `${grown.distance}px from the bottom`)
    if (grown.growerWasAboveTheViewport) {
      check('and the node they were reading did not move',
        grown.moved <= 2,
        `${grown.beforeTop.toFixed(1)}px -> ${grown.afterTop.toFixed(1)}px (moved ${grown.moved.toFixed(1)}px)`)
    } else {
      console.log('  skip the pixel check — this pane is too short to grow anything above the viewport')
    }
  }
}

// --- 2. scroll every pane to the top ----------------------------------------
console.log('\n2. scrolled to the top of every pane')
await scrollAllToTop()
await page.waitForTimeout(1500)
const scrolledUp = await measure()
for (const [name, m] of Object.entries(scrolledUp)) {
  check(`${name} stayed where it was put`, m.present && m.distance > THRESHOLD, describe(name, m))
}

// --- 3. switch session from the sidebar -------------------------------------
// This is the whole point of the canary. The panes are not remounted, so a
// pin that only runs on mount never runs again and the new log inherits
// "the user is not at the bottom" from the log they stopped looking at.
// The sidebar is newest-first and this host mints sessions all day, so a
// fixed index rots: run it tomorrow and it lands on a nine-row session where
// nothing overflows and there is nothing to be wrong about. Walk forward
// until a session actually overflows a pane, and say which one was used.
console.log('\n3. switching session from the sidebar')
let switched = null
let usedIndex = SIDEBAR_INDEX
for (let i = SIDEBAR_INDEX; i < SIDEBAR_INDEX + 8; i++) {
  const target = page.locator('.bc-session-item-main').nth(i)
  if (await target.count() === 0) break
  const label = (await target.textContent())?.trim().slice(0, 50)
  const previousId = await page.evaluate(() => new URL(location.href).searchParams.get('session'))
  await target.click({ timeout: 30000 })
  await page.waitForFunction(
    id => new URL(location.href).searchParams.get('session') !== id,
    previousId,
    { timeout: 60000 },
  )
  await page.waitForTimeout(6000)
  switched = await measure()
  usedIndex = i
  const overflowing = Object.values(switched).filter(m => m.present && m.overflow > THRESHOLD).length
  console.log(`  #${i}: ${label} — ${overflowing} pane(s) overflowing`)
  if (overflowing > 0) break
  // Nothing to measure here, so this pane switch has told us nothing. Scroll
  // back up before trying the next one, or the next switch starts pinned and
  // the whole point is lost.
  await scrollAllToTop()
  await page.waitForTimeout(1000)
}

let measurable = 0
for (const [name, m] of Object.entries(switched)) {
  if (!m.present) { check(`${name} rendered`, false, 'pane absent'); continue }
  if (m.overflow <= THRESHOLD) {
    console.log(`  skip ${name} — the new log fits in the pane (${m.overflow}px of overflow), nothing to be wrong about`)
    continue
  }
  measurable++
  check(`${name} opened at its newest end`, m.distance <= THRESHOLD, describe(name, m))
}
check('at least one pane overflowed, so the switch was measurable at all',
  measurable > 0, `${measurable} of ${Object.keys(PANES).length}`)

// --- 4. the other direction: filtering must not move the reader -------------
// Thread's filter chips rebuild the row list of the session the user is
// reading. That is a different list, but it is not a different log, and a pin
// that fires here yanks the page out from under them.
console.log('\n4. filtering the log being read must not jump to the bottom')
const chip = page.locator('.bc-thread-wrap .bc-filter-chip-on').first()
if (await chip.count() === 0) {
  console.log('  skip — no filter chip is on in this session')
} else {
  await page.evaluate(selector => { const el = document.querySelector(selector); if (el) el.scrollTop = 0 }, PANES.thread)
  await page.waitForTimeout(800)
  const before = (await measure()).thread
  await chip.click({ timeout: 30000 })
  await page.waitForTimeout(2500)
  const after = (await measure()).thread
  check('Thread is still scrolled up after a filter toggle',
    after.overflow <= THRESHOLD || after.distance > THRESHOLD,
    `${describe('before', before)} -> ${describe('after', after)}`)
  await chip.click({ timeout: 30000 })
  await page.waitForTimeout(1500)
}

// --- 5. a second switch, so the pin is not a one-shot -----------------------
// Still in the same page load: no navigation, no remount, the sidebar again.
console.log('\n5. scrolled up again, then a second sidebar switch')
await scrollAllToTop()
await page.waitForTimeout(1000)
const second = page.locator('.bc-session-item-main').nth(usedIndex + 1)
const secondLabel = (await second.textContent())?.trim().slice(0, 50)
const previousId = await page.evaluate(() => new URL(location.href).searchParams.get('session'))
console.log(`  switching to sidebar session #${usedIndex + 1}: ${secondLabel}`)
await second.click({ timeout: 30000 })
await page.waitForFunction(
  id => new URL(location.href).searchParams.get('session') !== id,
  previousId,
  { timeout: 60000 },
)
await page.waitForTimeout(6000)
const reopened = await measure()
for (const [name, m] of Object.entries(reopened)) {
  if (!m.present) { check(`${name} rendered`, false, 'pane absent'); continue }
  if (m.overflow <= THRESHOLD) {
    console.log(`  skip ${name} — the new log fits in the pane (${m.overflow}px of overflow)`)
    continue
  }
  check(`${name} opened at its newest end again`, m.distance <= THRESHOLD, describe(name, m))
}

await page.screenshot({ path: '/tmp/sticky-bottom-canary.png', fullPage: false })
console.log('\nscreenshot: /tmp/sticky-bottom-canary.png')
console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`)
await browser.close()
process.exit(failures === 0 ? 0 : 1)

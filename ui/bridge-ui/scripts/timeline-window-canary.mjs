// Live browser canary for the windowed chat panes.
//
//   DASH_TOKEN=$(...) node scripts/timeline-window-canary.mjs [<session_id>]
//
// `npm run check` renders these panes through react-dom/server, which proves
// what they render and cannot prove what the browser does with it. Everything
// the window's design rests on is browser behavior: revealing earlier blocks
// inserts content ABOVE the viewport, and the pane has no spacer to hold the
// reader's place, so it records the distance from the bottom and restores it
// in a layout effect. Whether that actually leaves the content still is a
// question only a real layout can answer.
//
// So the load-bearing check here is pixel-exact and deliberately not a
// selector: it keeps a handle to ONE live DOM node, clicks "Show earlier",
// and asserts that same node has not moved on screen while thousands of
// pixels of content appeared above it. Measured on the largest session on
// this host: 0.3px of movement against 18,422px inserted.
//
// Not wired into `npm run check`: it needs a deployed dash, a token, and
// Playwright. It borrows dash's Playwright install rather than adding one
// here — bridge-ui is a library and should not carry a browser.
//
// â  Selector traps, both of which cost a run to find:
//   - Only the Turns pane is on by default. The other two are behind
//     `.bc-pane-toggle[aria-label="Timeline"|"Thread"]`.
//   - `.bc-pane-earlier` and `.bc-jump-latest` each exist in MORE THAN ONE
//     pane. Always scope to `.bc-timeline` or `.bc-thread-wrap`.
import { chromium } from '/home/kayushkincom/repos/dash/node_modules/playwright/index.mjs'

const TOKEN = process.env.DASH_TOKEN
const SESSION = process.argv[2] || 'br_1785171126409277953'
const BASE = 'https://dash.kayushkin.com'

let failures = 0
const check = (name, cond, detail) => {
  if (cond) console.log(`  ok   ${name}${detail ? ` — ${detail}` : ''}`)
  else { console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ''}`); failures++ }
}

const browser = await chromium.launch({ headless: true })
const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } })
await ctx.addCookies([{ name: 'dash_token', value: TOKEN, domain: 'dash.kayushkin.com', path: '/' }])
const page = await ctx.newPage()
page.on('console', m => { if (m.type() === 'error') console.log(`  [console.error] ${m.text().slice(0, 200)}`) })

console.log(`opening ${BASE}/?session=${SESSION}`)
await page.goto(`${BASE}/?session=${SESSION}`, { waitUntil: 'domcontentloaded', timeout: 120000 })

// The log has to finish loading before any of this means anything.
// Only the Turns pane is on by default; the history has to have arrived
// before the other two can render anything.
await page.waitForSelector('.bc-turns-item', { timeout: 300000 })
console.log('  history is in; showing the Timeline and Thread panes')
await page.locator('.bc-pane-toggle[aria-label="Timeline"]').click({ timeout: 30000 })
await page.locator('.bc-pane-toggle[aria-label="Thread"]').click({ timeout: 30000 })
await page.waitForSelector('.bc-timeline .bc-tl-item', { timeout: 120000 })
await page.waitForTimeout(3000)

// --- 1. the window is on ----------------------------------------------------
const read = () => page.evaluate(() => {
  const body = document.querySelector('.bc-timeline-body')
  const control = document.querySelector('.bc-timeline .bc-pane-earlier')
  const countText = control?.querySelector('.bc-pane-earlier-count')?.textContent || ''
  return {
    headerCount: Number((document.querySelector('.bc-timeline-count')?.textContent || '0').replace(/[^0-9]/g, '')),
    items: document.querySelectorAll('.bc-timeline .bc-tl-item').length,
    hasControl: !!control,
    buttons: document.querySelectorAll('.bc-timeline .bc-pane-earlier-btn').length,
    earlierCount: Number((countText.match(/[\d,]+/)?.[0] || '0').replace(/,/g, '')),
    scrollHeight: body?.scrollHeight ?? 0,
    scrollTop: body?.scrollTop ?? 0,
    clientHeight: body?.clientHeight ?? 0,
  }
})

const before = await read()
console.log(`\nTimeline window — live, ${SESSION}`)
check('the pane renders far fewer items than the session holds',
  before.items > 0 && before.items < before.headerCount / 2,
  `${before.items} rendered of ${before.headerCount} in the header`)
check('the header still counts the whole session', before.headerCount > 1000, `${before.headerCount}`)
check('the earlier-events control renders inside the timeline', before.hasControl)
check('and offers two buttons', before.buttons === 2, `${before.buttons}`)
check('the earlier count plus the rendered items is the session',
  before.earlierCount + before.items === before.headerCount,
  `${before.earlierCount} + ${before.items} = ${before.earlierCount + before.items} vs ${before.headerCount}`)

// --- 2. the load-bearing check ----------------------------------------------
// Track ONE live DOM node across the reveal. Not a selector — the same node.
const moved = await page.evaluate(async () => {
  const body = document.querySelector('.bc-timeline-body')
  const items = [...document.querySelectorAll('.bc-timeline .bc-tl-item')]
  // Something in the middle of the viewport, so it is genuinely on screen.
  const rect = body.getBoundingClientRect()
  const onScreen = items.filter(el => {
    const r = el.getBoundingClientRect()
    return r.top >= rect.top && r.bottom <= rect.bottom
  })
  const node = onScreen[Math.floor(onScreen.length / 2)] || items[items.length - 1]
  const beforeTop = node.getBoundingClientRect().top
  const beforeDistance = body.scrollHeight - body.scrollTop
  const beforeScrollHeight = body.scrollHeight
  const beforeItems = items.length

  document.querySelector('.bc-timeline .bc-pane-earlier-btn').click()
  // Let React commit and the layout effect restore the scroll position.
  await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))
  await new Promise(r => setTimeout(r, 400))

  return {
    stillMounted: node.isConnected,
    beforeTop, afterTop: node.getBoundingClientRect().top,
    beforeDistance, afterDistance: body.scrollHeight - body.scrollTop,
    insertedPx: body.scrollHeight - beforeScrollHeight,
    beforeItems, afterItems: document.querySelectorAll('.bc-timeline .bc-tl-item').length,
  }
})

console.log('\nrevealing earlier items')
check('the tracked node survived the reveal', moved.stillMounted)
check('and did not move on screen', Math.abs(moved.afterTop - moved.beforeTop) <= 1,
  `${moved.beforeTop.toFixed(1)}px -> ${moved.afterTop.toFixed(1)}px (moved ${(moved.afterTop - moved.beforeTop).toFixed(1)}px)`)
check('the distance from the bottom is unchanged',
  Math.abs(moved.afterDistance - moved.beforeDistance) <= 1,
  `${moved.beforeDistance} -> ${moved.afterDistance}`)
check('content really was inserted above it', moved.insertedPx > 500, `${moved.insertedPx}px`)
check('more items are rendered', moved.afterItems > moved.beforeItems,
  `${moved.beforeItems} -> ${moved.afterItems}`)

const after = await read()
check('the earlier count fell by exactly the items that appeared',
  before.earlierCount - after.earlierCount === after.items - before.items,
  `earlier ${before.earlierCount} -> ${after.earlierCount} (-${before.earlierCount - after.earlierCount}), items +${after.items - before.items}`)
check('the header count did not change', after.headerCount === before.headerCount,
  `${before.headerCount} -> ${after.headerCount}`)

// --- 3. show all ------------------------------------------------------------
console.log('\nshow all')
await page.locator('.bc-timeline .bc-pane-earlier-all').click()
await page.waitForTimeout(4000)
const all = await read()
check('renders the whole session', all.items === all.headerCount, `${all.items} of ${all.headerCount}`)
check('and the control is gone', !all.hasControl)

// --- 4. Thread regression (its control was renamed) -------------------------
console.log('\nThread — the renamed control')
const thread = await page.evaluate(() => {
  const el = document.querySelector('.bc-thread-wrap .bc-pane-earlier')
  if (!el) return { present: false }
  const cs = getComputedStyle(el)
  const btn = el.querySelector('.bc-pane-earlier-btn')
  const bcs = btn ? getComputedStyle(btn) : null
  return {
    present: true,
    borderStyle: cs.borderTopStyle,
    borderWidth: cs.borderTopWidth,
    padding: cs.paddingTop,
    buttons: el.querySelectorAll('.bc-pane-earlier-btn').length,
    btnRadius: bcs?.borderTopLeftRadius,
    text: el.textContent.slice(0, 80),
  }
})
check('Thread renders the renamed control', thread.present, JSON.stringify(thread))
check('and it is still styled — dashed border', thread.borderStyle === 'dashed', `${thread.borderStyle} ${thread.borderWidth}`)
check('its buttons are still pills', thread.btnRadius === '999px', `${thread.btnRadius}`)
check('with both buttons', thread.buttons === 2, `${thread.buttons}`)

await page.screenshot({ path: '/tmp/timeline-canary.png', fullPage: false })
console.log('\nscreenshot: /tmp/timeline-canary.png')
console.log(failures === 0 ? `\nALL PASS` : `\n${failures} FAILED`)
await browser.close()
process.exit(failures === 0 ? 0 : 1)

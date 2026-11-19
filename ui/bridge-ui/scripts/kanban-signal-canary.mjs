// Live browser canary for the signal mount in the kanban card drawer.
//
//   node scripts/kanban-signal-canary.mjs [<card_id>]
//
// What it proves that `tsc` cannot: that opening a card whose todo has open
// signals renders them, that the actions offered match the signal's kind, and
// that clicking one posts the right verb and clears the card.
//
// How it is wired, and why:
//   - /api/kanban keeps going to the LIVE kanban-store on :8305, so the board,
//     the columns and the card are real records with their real shapes. The
//     canary only READS them; it creates and deletes nothing.
//   - the bridge base is pointed at a stub served here instead of the live
//     gateway on :8160, because the live gateway has no /signals route yet —
//     that is the deploy this whole epic is gated on. The stub answers the one
//     read and the one verb the drawer uses, in the shapes
//     llm-bridge/msg/signal.go defines, and RECORDS what the client posted so
//     the check can assert on the request rather than on a rendered guess.
//
// Not wired into any npm script: it needs a browser and a running dev server.
// It borrows dash's Playwright install rather than adding one here — bridge-ui
// is a library and should not carry a browser.
//
// ⚠ Traps, each of which cost a run:
//   - the drawer is a fixed overlay; the card tile behind it still matches
//     `.bk-card`, so assert on `.bk-drawer` for anything drawer-shaped.
//   - the board polls every 15s and re-renders the columns. Grab text, not
//     element handles, across any wait.
//   - a cross-origin bridge base needs CORS, including a preflight answer for
//     the POST — without it the click fails silently in the page.
import { createServer } from 'node:http'
import { chromium } from '/home/kayushkincom/repos/dash/node_modules/playwright/index.mjs'

const CARD_ID = process.argv[2] || '20624c9a-1e6a-453d-969e-e9a2fd182e51'
const APP = process.env.APP || 'http://127.0.0.1:5199'
const STUB_PORT = Number(process.env.STUB_PORT || 18771)

let failures = 0
const check = (name, cond, detail) => {
  if (cond) console.log(`  ok   ${name}${detail ? ` — ${detail}` : ''}`)
  else { console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ''}`); failures++ }
}

// Two open signals against the one card: a derived question (what an
// autonomous worker raises, and the reason surface "kanban" exists) and a
// notification. They exercise the two different action sets the card offers.
const QUESTION = {
  id: 'sig-question-1',
  session_id: 'br_canary_worker',
  kind: 'question',
  source: 'derived',
  surface: 'kanban',
  title: 'The migration needs a column dropped. Drop it?',
  body: 'The worker stopped here and is waiting.',
  options: [
    { label: 'Drop it', value: 'drop', description: 'Data in that column is gone' },
    { label: 'Leave it', value: 'leave' },
  ],
  allow_freeform: true,
  state: 'open',
  linked_todo_id: CARD_ID,
  created_at: '2026-08-01T18:00:00Z',
}
const NOTIFICATION = {
  id: 'sig-notification-1',
  session_id: 'br_canary_worker',
  kind: 'notification',
  source: 'derived',
  surface: 'kanban',
  title: 'Deploy finished',
  severity: 'info',
  state: 'open',
  linked_todo_id: CARD_ID,
  created_at: '2026-08-01T18:01:00Z',
}

const open = new Map([[QUESTION.id, QUESTION], [NOTIFICATION.id, NOTIFICATION]])
const posted = []

const stub = createServer(async (req, res) => {
  const url = new URL(req.url, 'http://stub')
  // The app's fetch sends credentials, and a credentialed request refuses a
  // wildcard origin — so echo the caller's.
  res.setHeader('Access-Control-Allow-Origin', req.headers.origin || '*')
  res.setHeader('Access-Control-Allow-Credentials', 'true')
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type')
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS')
  if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }

  if (req.method === 'GET' && url.pathname === '/signals') {
    // The server narrows by linked_todo_id and refuses an empty one with a
    // 400 — mirrored here so a client that forgets the id fails loudly.
    const todo = url.searchParams.get('linked_todo_id')
    if (url.searchParams.has('linked_todo_id') && !todo) {
      res.writeHead(400); res.end('linked_todo_id must not be empty'); return
    }
    const rows = [...open.values()].filter(s => !todo || s.linked_todo_id === todo)
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(rows))
    return
  }

  const resolve = url.pathname.match(/^\/signals\/([^/]+)\/resolve$/)
  if (req.method === 'POST' && resolve) {
    const id = decodeURIComponent(resolve[1])
    let raw = ''
    for await (const chunk of req) raw += chunk
    const body = JSON.parse(raw || '{}')
    posted.push({ id, state: body.state })
    const row = open.get(id)
    if (!row) { res.writeHead(404); res.end('no such signal'); return }
    // The real verb refuses "acknowledged" for a question. Refusing it here
    // too means a UI that offered the wrong button would fail the canary.
    if (body.state === 'acknowledged' && row.kind === 'question') {
      res.writeHead(400); res.end('a question cannot be acknowledged'); return
    }
    open.delete(id)
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ ...row, state: body.state, resolved_at: '2026-08-01T18:05:00Z' }))
    return
  }

  // Everything else the page happens to ask the bridge for. 404 is what a
  // gateway without the feature answers, and every read must survive it.
  res.writeHead(404); res.end('not found')
})

await new Promise(r => stub.listen(STUB_PORT, '127.0.0.1', r))
console.log(`signals stub on :${STUB_PORT}, ${open.size} open against card ${CARD_ID}`)

const browser = await chromium.launch()
const page = await browser.newPage()
page.on('console', m => { if (m.type() === 'error') console.log(`  [page error] ${m.text()}`) })

try {
  await page.goto(`${APP}/kanban`, { waitUntil: 'domcontentloaded' })
  await page.waitForSelector('.bk-card', { timeout: 30000 })

  // The badge is the board-level surface and P5 owns it. This canary must not
  // change it, so record what it shows before opening anything.
  const badgeCount = await page.locator('.bk-card-signal').count()
  check('the tile badge still renders', badgeCount > 0, `${badgeCount} badge(s) on the board`)

  const tile = page.locator('.bk-card', { hasText: 'TEST: gate smoke card' }).first()
  await tile.click()
  await page.waitForSelector('.bk-drawer', { timeout: 10000 })

  const signals = page.locator('.bk-drawer .bk-drawer-signals')
  check('the drawer mounts a signals section', await signals.count() === 1)
  check('the section is above the editor', await page.locator('.bk-drawer-signals ~ .bk-drawer-label').count() > 0)

  const cards = page.locator('.bk-drawer .bc-signal-card')
  check('both open signals render', await cards.count() === 2, `${await cards.count()} card(s)`)
  const text = await signals.innerText()
  check('the question is readable', text.includes('The migration needs a column dropped'))
  check('its options render', text.includes('Drop it') && text.includes('Leave it'))
  check('the notification is readable', text.includes('Deploy finished'))

  // Constraint: a question is never acknowledged, only answered or dismissed.
  const questionCard = page.locator('.bc-signal-request', { hasText: 'The migration needs' })
  const notificationCard = page.locator('.bc-signal-request', { hasText: 'Deploy finished' })
  check('a question offers no Acknowledge',
    await questionCard.locator('.bc-signal-ack').count() === 0)
  check('a notification does offer Acknowledge',
    await notificationCard.locator('.bc-signal-ack').count() === 1)
  check('a derived question offers Dismiss',
    await questionCard.locator('.bc-signal-dismiss').count() === 1)
  check('a derived question offers no Decline (nothing was parked to deny)',
    await questionCard.locator('.bc-signal-decline').count() === 0)
  check('answering is gated until a question is answered',
    await questionCard.locator('.bc-signal-submit').isDisabled())

  // Picking an option must enable the send. This is the whole reason the
  // drawer mounts the card and not the badge.
  await questionCard.locator('.bc-signal-option').first().click()
  check('picking an option enables the send',
    !(await questionCard.locator('.bc-signal-submit').isDisabled()))

  // The radios are inside a drawer that stretches every input to full width.
  const radioWidth = await questionCard.locator('input[type="radio"]').first()
    .evaluate(el => el.getBoundingClientRect().width)
  check('the drawer does not stretch the option radios', radioWidth < 40, `${Math.round(radioWidth)}px`)

  await notificationCard.locator('.bc-signal-ack').click()
  await page.waitForFunction(
    () => document.querySelectorAll('.bk-drawer .bc-signal-card').length === 1,
    null, { timeout: 10000 },
  )
  check('acknowledging posts the right verb',
    posted.some(p => p.id === NOTIFICATION.id && p.state === 'acknowledged'),
    JSON.stringify(posted))
  check('the acknowledged notification leaves the drawer',
    !(await signals.innerText()).includes('Deploy finished'))
  check('the unanswered question stays',
    (await signals.innerText()).includes('The migration needs a column dropped'))

  await questionCard.locator('.bc-signal-dismiss').click()
  await page.waitForFunction(
    () => document.querySelectorAll('.bk-drawer .bk-drawer-signals').length === 0,
    null, { timeout: 10000 },
  )
  check('dismissing posts the right verb',
    posted.some(p => p.id === QUESTION.id && p.state === 'dismissed'),
    JSON.stringify(posted))
  check('an empty signal set renders no section at all',
    await page.locator('.bk-drawer .bk-drawer-signals').count() === 0)
  check('the drawer itself is still open and editable',
    await page.locator('.bk-drawer input').first().isVisible())
} finally {
  await browser.close()
  stub.close()
}

console.log(failures === 0 ? '\nPASS' : `\n${failures} FAILED`)
process.exit(failures === 0 ? 0 : 1)

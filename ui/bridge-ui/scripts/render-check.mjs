// Render checks for the presentational chat components.
//
// Run with `npm run check`. esbuild bundles this file (which is why it can
// import .ts/.tsx from src directly) and node runs the bundle, so there is
// no test framework to install and nothing to keep in sync with the build.
//
// It asserts on markup from react-dom/server, so it proves what a component
// renders for a given input — not that the app wires that input up. The
// browser canary is what proves the wiring. Both are worth having: a browser
// run cannot be sabotaged cheaply, and this can.
//
// Currently covers the spend-ceiling surfaces. Extend it rather than adding
// a second mechanism.
import { renderToStaticMarkup } from 'react-dom/server'
import { createElement as h } from 'react'
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import {
  BridgeOrchestrator, CostBreakdown, GitPanel, LinkedKanbanPanel,
  OrchestratorPanel,
} from '../src/index.ts'
import { CeilingEditor } from '../src/components/chat/CostBreakdown.tsx'
import { ServerTabStrip, ConnectedServerSessionRow } from '../src/components/chat/ConnectedServerSidebar.tsx'
import NewSessionMenu from '../src/components/chat/NewSessionMenu.tsx'
import { ConnectedServersContext } from '../src/components/chat/ConnectedServers.tsx'
import { RefChip } from '../src/components/chat/RefChip.tsx'
import { RefChipServerContext } from '../src/components/chat/refChipServer.ts'
import { SharedFileCard, SharedImageGrid } from '../src/components/chat/SharedFiles.tsx'
import { SessionFilesContext } from '../src/components/chat/SessionFileChip.tsx'
import { ConnectedServerThread } from '../src/components/chat/ConnectedServerThread.tsx'
import { peerConnectionBridgeConfig, CONNECTED_SERVER_PATH_SOURCES } from '../src/peers.ts'
import { TextWithReferenceLinks } from '../src/components/chat/ReferenceText.tsx'
import { kanbanPollWouldFetch, preserveUnchangedKanbanPayload } from '../src/useKanban.ts'
import { indexPrincipalsByID, pickablePrincipals, principalInitials, principalIsDisabled } from '../src/usePrincipals.ts'
import { CardDetail } from '../src/components/BridgeKanban.tsx'
import {
  BridgePrincipals, MembershipsSection, PrincipalDetailView, PrincipalListView,
} from '../src/components/BridgePrincipals.tsx'
import { principalsSearchURL } from '../src/principalStoreClient.ts'
import { effectiveGrantsURL } from '../src/grantStoreClient.ts'
import { dispatchInstanceChoices, partitionGrantRows, relationWording, resourceTypeWording } from '../src/grantResources.ts'
import { ResourceGroup } from '../src/components/PrincipalGrants.tsx'
import { BridgeBundles, BundleCard } from '../src/components/BridgeBundles.tsx'
import { SharedPoll, loadJSONList, sharedPoll } from '../src/sharedPoll.ts'
import { bridgePrefsStoreFor, mergePrefs, reconcilePrefs } from '../src/bridgePrefsStore.ts'
import { harnessMapOf, harnessNameKey, harnessNamesFromKey, harnessesPoll } from '../src/useBridgeHarnesses.ts'
import { initialSessionDeeplinkState, readSessionDeeplink, writeSessionParam } from '../src/sessionDeeplink.ts'
import { readAgentPrompt, stripAgentPrompt, writeAgentPrompt, suggestAgentPrompt } from '../src/agentPrompt.ts'
import { dispatchAgentOnCard } from '../src/agentDispatch.ts'
import { BridgeContext, DEFAULT_BRIDGE_ROUTES } from '../src/context.ts'
import { PeerMessageCard, approvalWayPoll } from '../src/components/PeerMessageCards.tsx'
import { BridgeConformance } from '../src/components/BridgeConformance.tsx'
import { applySessionAggregates, sessionTokenTotalsAreMissing } from '../src/components/BridgeSessions.tsx'
import { BridgeSettings } from '../src/components/BridgeSettings.tsx'
import { BridgePeers } from '../src/components/BridgePeers.tsx'
import { composerAutoGrowHeightPx } from '../src/components/chat/composerAutoGrow.ts'
import { StatusDot } from '../src/components/chat/StatusDot.tsx'
import { MemoryRouter } from 'react-router-dom'
import { BridgeLayout } from '../src/components/BridgeLayout.tsx'
import { MinimalChromeProvider } from '../src/components/minimal/MinimalChromeContext.tsx'
import { SplitDragHandle } from '../src/components/chat/SplitDragHandle.tsx'
import {
  EVEN_SPLIT_GROW_UNITS, MINIMUM_PANE_PIXELS, measureSplitDragGeometry, splitGrowUnitsAfterDrag,
} from '../src/components/chat/splitDragGeometry.ts'
import { groupSignalsByRequest, ChatContext, ChatProvider } from '@kayushkin/chat-core'
import { SessionActionEntry, SessionActionInText, SessionActionsContext } from '../src/components/chat/SessionActions.tsx'
import { BridgeMachines, HistoryCharts, MachineStrip } from '../src/components/machines/BridgeMachines.tsx'
import { machineStatsOverviewPoll, peerConnectionsPoll, serverDescriptionPoll } from '../src/useMachineStats.ts'
import { SessionsToFileList } from '../src/components/chat/SessionsToFile.tsx'
import { machineStatsFixtureFetch, machineStatsFixtureHistory, machineStatsFixtureOverview } from '../test/fixtures/machineStatsFixture.ts'

let failures = 0
const check = (name, cond, detail) => {
  if (cond) console.log(`  ok   ${name}`)
  else { console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ''}`); failures++ }
}

console.log('CostBreakdown')
{
  const html = renderToStaticMarkup(h(CeilingEditor, { spendUSD: 10, maxBudgetUSD: 25, setCeiling: async () => null }))
  check('ceiling editor starts on the ceiling in force', html.includes('value="25"'), html)
  check('ceiling editor with a ceiling says "Change ceiling"', html.includes('Change ceiling'), html)
}
{
  const html = renderToStaticMarkup(h(CeilingEditor, { spendUSD: 10, maxBudgetUSD: 26.286004249999998, setCeiling: async () => null }))
  check('ceiling editor rounds a stored ceiling to cents', html.includes('value="26.29"'), html)
}
{
  const html = renderToStaticMarkup(h(CeilingEditor, { spendUSD: 0, maxBudgetUSD: 0, setCeiling: async () => null }))
  check('ceiling editor with no ceiling starts empty and says "Set a ceiling"', html.includes('value=""') && html.includes('Set a ceiling'), html)
}
{
  const html = renderToStaticMarkup(h(CostBreakdown, {
    rows: [], fallbackTotalUSD: 0, ceiling: { spendUSD: 3, maxBudgetUSD: 10 },
  }))
  check('under a ceiling renders "$3.00 / $10.00"', html.includes('$3.00 / $10.00'), html)
  check('under 70% carries no tone class', !html.includes('bc-cost-ceiling-warn') && !html.includes('bc-cost-ceiling-crit'), html)
}
{
  const html = renderToStaticMarkup(h(CostBreakdown, {
    rows: [], fallbackTotalUSD: 0, ceiling: { spendUSD: 3, maxBudgetUSD: 2.5 },
  }))
  check('over the ceiling renders "$3.00 / $2.50"', html.includes('$3.00 / $2.50'), html)
  check('over the ceiling is crit', html.includes('bc-cost-ceiling-crit'), html)
}
{
  const html = renderToStaticMarkup(h(CostBreakdown, {
    rows: [], fallbackTotalUSD: 0, ceiling: { spendUSD: 7.5, maxBudgetUSD: 10 },
  }))
  check('at 75% of the ceiling is warn', html.includes('bc-cost-ceiling-warn'), html)
}
{
  // A ceiling with no api_call telemetry is the state a freshly-loaded
  // session is in, and it must still open its drill-down. This branch used
  // to render a bare span, which made the ceiling rows unreachable in
  // exactly the case that reaches it.
  const html = renderToStaticMarkup(h(CostBreakdown, {
    rows: [], fallbackTotalUSD: 0, ceiling: { spendUSD: 3, maxBudgetUSD: 10 },
  }))
  check('ceiling with no telemetry is still clickable', html.includes('bc-cost-clickable') && html.includes('bc-cost-caret'), html)
}
{
  // Nothing spent is "$0.00", not "$0.0000": zero is not a sub-cent
  // quantity and four decimals read as a precise measurement.
  const html = renderToStaticMarkup(h(CostBreakdown, {
    rows: [], fallbackTotalUSD: 0, ceiling: { spendUSD: 0, maxBudgetUSD: 10 },
  }))
  check('nothing spent reads "$0.00 / $10.00"', html.includes('$0.00 / $10.00'), html)
}
{
  // No ceiling and no telemetry: the chip must be exactly what it was before
  // this change existed, i.e. nothing at all.
  const html = renderToStaticMarkup(h(CostBreakdown, { rows: [], fallbackTotalUSD: 0 }))
  check('no ceiling + no cost renders nothing', html === '', JSON.stringify(html))
}
{
  const html = renderToStaticMarkup(h(CostBreakdown, { rows: [], fallbackTotalUSD: 1.25 }))
  check('no ceiling + fallback cost is unchanged', html.includes('$1.25') && !html.includes('bc-cost-ceiling'), html)
}
console.log('\nkanbanPollWouldFetch')
{
  // These four cases are the guards at useKanban.ts fetchBoards:62 and
  // fetchView:75, restated. If either guard changes, this must change with it —
  // a poll that is scheduled but returns at its first line is a timer doing
  // nothing, which is what the chat pane was running.
  check('the chat pane shape fetches nothing', !kanbanPollWouldFetch(true, false, null))
  check('a board id alone is worth polling', kanbanPollWouldFetch(true, false, 'board_1'))
  check('loading the board list is worth polling', kanbanPollWouldFetch(true, true, null))
  check('disabled never polls', !kanbanPollWouldFetch(false, true, 'board_1'))
}

console.log('\npreserveUnchangedKanbanPayload')
{
  // LinkedKanbanPanel now re-reads its session's cards every 15 seconds,
  // because kanban-store has no notifier and the curator moves cards under an
  // open pane. Most of those reads bring back exactly what is already shown, so
  // the identity guard is what keeps a poll from re-rendering the chat pane
  // once a tick forever. If this stops returning the previous reference for an
  // unchanged payload, the fix turns into a permanent render loop.
  const shown = [{ card_id: 'c1', item: { title: 'Ship it', status: 'open' } }]
  const sameFromServer = [{ card_id: 'c1', item: { title: 'Ship it', status: 'open' } }]
  check('an unchanged payload keeps the previous reference',
    preserveUnchangedKanbanPayload(shown, sameFromServer) === shown)

  const moved = [{ card_id: 'c1', item: { title: 'Ship it', status: 'done' } }]
  check('a card that moved is a new reference',
    preserveUnchangedKanbanPayload(shown, moved) === moved)

  const added = [...shown, { card_id: 'c2', item: { title: 'Second', status: 'open' } }]
  check('a newly linked card is a new reference',
    preserveUnchangedKanbanPayload(shown, added) === added)

  // The empty cases are the ones a failing poll used to reach: before this
  // pass, listCardsForEntity returned [] for an HTTP error, so a blip looked
  // exactly like "this session has no cards". It throws now, and the panel
  // keeps its list — but an honestly empty answer still has to land.
  check('an emptied list is a new reference', preserveUnchangedKanbanPayload(shown, []).length === 0)
  const alreadyEmpty = []
  check('two empty lists keep the previous reference',
    preserveUnchangedKanbanPayload(alreadyEmpty, []) === alreadyEmpty)

  const tags = [{ tag: 'kanban-do-not-track' }]
  check('unchanged tags keep the previous reference',
    preserveUnchangedKanbanPayload(tags, [{ tag: 'kanban-do-not-track' }]) === tags)
  check('a removed tag is a new reference',
    preserveUnchangedKanbanPayload(tags, []) !== tags)
}
console.log('\nsessionDeeplink')
{
  // A URL with no param says nothing, and a restored session claims the bar.
  let st = initialSessionDeeplinkState
  let r = readSessionDeeplink(null, st)
  check('an absent param opens nothing', r.open === null)
  st = r.state
  let w = writeSessionParam('br_restored', st)
  check('a restored session is written into the URL', w.write === true && w.value === 'br_restored')
  st = w.state
  check('and writing again is a no-op', writeSessionParam('br_restored', st).write === false)
  check('our own param is not read back as a deeplink', readSessionDeeplink('br_restored', st).open === null)
}
{
  // The regression that mattered: the write effect sees the pre-deeplink focus
  // in the same commit the read fired in.
  let st = { applied: 'br_open', awaiting: null }
  const r = readSessionDeeplink('br_link', st)
  check('an inbound deeplink opens', r.open === 'br_link')
  st = r.state
  const stale = writeSessionParam('br_open', st)
  check('the stale focus does not overwrite the deeplink', stale.write === false)
  st = stale.state
  check('the deeplink is still pending', st.awaiting === 'br_link')
  const landed = writeSessionParam('br_link', st)
  check('and when focus lands the URL needs no write', landed.write === false)
  st = landed.state
  check('the wait is over once it lands', st.awaiting === null)
  const next = writeSessionParam('br_other', st)
  check('a later session change does write', next.write === true && next.value === 'br_other')
}
{
  // A pending "new chat" (or no pane at all) must not leave a stale id behind.
  const st = { applied: 'br_gone', awaiting: null }
  const w = writeSessionParam(null, st)
  check('a session-less focus clears the param', w.write === true && w.value === null)
  check('and nothing is left applied', w.state.applied === null)
}
{
  // An in-app /?session=<id> link has to work more than once per page load —
  // the old once-only ref meant the second link click did nothing.
  let st = { applied: 'br_a', awaiting: null }
  st = writeSessionParam('br_a', st).state
  const second = readSessionDeeplink('br_b', st)
  check('a second in-app deeplink still opens', second.open === 'br_b')
}

// SharedPoll — the store behind useBridgeInstances/useBridgeMachines. These
// checks drive it directly rather than through React, because what they need
// to pin is how many requests and timers N subscribers cost, and that is
// invisible in rendered markup.
async function sharedPollChecks() {
  const settle = () => new Promise(resolve => setTimeout(resolve, 0))
  const after = ms => new Promise(resolve => setTimeout(resolve, ms))

  console.log('SharedPoll — one poll, many subscribers')
  {
    let loads = 0
    const poll = new SharedPoll(async () => { loads++; return { ok: true, value: [{ id: 'a' }] } }, [], 20)
    check('nobody watching means no request', loads === 0 && poll.polling === false)

    const notified = [0, 0, 0]
    const off = [0, 1, 2].map(i => poll.subscribe(() => { notified[i]++ }))
    await settle()
    check('three subscribers cost one request', loads === 1, `loads=${loads}`)
    check('and one timer', poll.polling === true)
    check('all three saw the answer', notified.every(n => n === 1), JSON.stringify(notified))
    check('and read the same array', poll.getSnapshot().data.length === 1 && poll.getSnapshot().loading === false)

    // A fourth component mounting later must not pay for its own request.
    const late = poll.subscribe(() => {})
    await settle()
    check('a late subscriber reuses the answer', loads === 1, `loads=${loads}`)
    late()

    // The interval belongs to the store, so it ticks once for everyone.
    await after(70)
    const ticked = loads
    check('the shared timer ticks once per interval', ticked >= 2 && ticked <= 5, `loads=${ticked}`)

    // Two components can pass the same callback. A Set collapses them into one
    // entry, so counting entries would stop the timer under a live subscriber.
    const same = () => {}
    const offSameA = poll.subscribe(same)
    const offSameB = poll.subscribe(same)
    offSameA()
    check('two subscribers sharing a callback still count as two', poll.subscriberCount === 4, `count=${poll.subscriberCount}`)
    offSameB()

    off[0]()
    off[0]() // React can unsubscribe twice; the count must not go negative.
    off[1]()
    check('still polling while one subscriber remains', poll.polling === true, `count=${poll.subscriberCount}`)
    check('and a repeated unsubscribe did not double-count', poll.subscriberCount === 1, `count=${poll.subscriberCount}`)
    off[2]()
    check('the last one out stops the timer', poll.polling === false && poll.subscriberCount === 0)

    const idle = loads
    await after(70)
    check('and no request fires once nobody is watching', loads === idle, `loads=${loads} idle=${idle}`)

    // Coming back (a tab switch, a route change) renders the last answer at
    // once instead of flashing empty, and refreshes behind it.
    const back = poll.subscribe(() => {})
    check('resubscribing keeps the cached answer', poll.getSnapshot().data.length === 1 && poll.getSnapshot().loading === false)
    await settle()
    check('and refreshes it', loads === idle + 1, `loads=${loads}`)
    back()
  }

  console.log('SharedPoll — what a subscriber sees')
  {
    let payload = [{ id: 'a' }]
    let fail = null
    const poll = new SharedPoll(async () => (fail ? { ok: false, error: fail, failure: 'refused' } : { ok: true, value: payload }), [], 100000)
    let notifications = 0
    const off = poll.subscribe(() => { notifications++ })
    await settle()
    const first = poll.getSnapshot().data
    check('the first answer arrives', first.length === 1 && notifications === 1)

    // Re-serving the same JSON must keep the old array, or every consumer's
    // useMemo recomputes and the pane re-renders on every tick for nothing.
    payload = [{ id: 'a' }]
    await poll.refresh()
    check('an unchanged payload keeps the old array', poll.getSnapshot().data === first)
    check('and notifies nobody', notifications === 1, `notifications=${notifications}`)

    payload = [{ id: 'a' }, { id: 'b' }]
    await poll.refresh()
    check('a changed payload replaces it', poll.getSnapshot().data.length === 2)
    check('and notifies once', notifications === 2, `notifications=${notifications}`)

    const good = poll.getSnapshot().data
    fail = 'HTTP 503'
    await poll.refresh()
    check('a failed refresh keeps the last good data', poll.getSnapshot().data === good)
    check('and reports the error', poll.getSnapshot().error === 'HTTP 503')
    check('and does not go back to loading', poll.getSnapshot().loading === false)

    fail = null
    await poll.refresh()
    check('recovering clears the error', poll.getSnapshot().error === null)
    off()
  }

  console.log('SharedPoll — concurrent refreshes')
  {
    let loads = 0
    // Every in-flight load is held here, not just the newest: if the dedupe
    // ever breaks, this check must still finish and report, not deadlock on a
    // request nobody kept a handle to.
    const held = []
    const poll = new SharedPoll(async () => {
      loads++
      await new Promise(resolve => held.push(resolve))
      return { ok: true, value: [] }
    }, [], 100000)
    const releaseAll = () => { while (held.length) held.pop()() }

    // Three components each awaiting a refresh after a write used to be three
    // GETs of the same URL.
    const all = Promise.all([poll.refresh(), poll.refresh(), poll.refresh()])
    await settle()
    check('three concurrent refreshes share one request', loads === 1, `loads=${loads}`)
    releaseAll()
    await all
    // Sharing is only for requests in flight — once one lands, the next
    // refresh must actually go to the server.
    const next = poll.refresh()
    await settle()
    releaseAll()
    await next
    check('and a later refresh is a new request', loads === 2, `loads=${loads}`)
  }

  console.log('SharedPoll — loading, and a load that throws')
  {
    const poll = new SharedPoll(async () => { throw new Error('boom') }, [], 100000)
    check('loading is true before the first attempt settles', poll.getSnapshot().loading === true)
    await poll.refresh()
    check('a throwing load does not stay loading', poll.getSnapshot().loading === false)
    check('and is reported, not swallowed', poll.getSnapshot().error === 'Error: boom', String(poll.getSnapshot().error))
  }

  console.log('SharedPoll — a request that never got an answer')
  {
    // A phone on mobile data drops one request; the retry a moment later works.
    let answers = ['unreachable', 'ok']
    let loads = 0
    const poll = new SharedPoll(async () => {
      loads++
      const next = answers.shift() ?? 'ok'
      return next === 'ok' ? { ok: true, value: [{ id: 'a' }] } : { ok: false, error: 'TypeError: Failed to fetch', failure: 'unreachable' }
    }, [], 100000, [10, 10, 10])
    const seen = []
    const off = poll.subscribe(() => { seen.push(poll.getSnapshot().error) })
    await settle()
    check('one dropped request shows no error', poll.getSnapshot().error === null, String(poll.getSnapshot().error))
    check('and settles loading', poll.getSnapshot().loading === false)
    await after(30)
    check('the store asks again within the retry delay', loads === 2, `loads=${loads}`)
    check('and has the answer', poll.getSnapshot().data.length === 1)
    check('no subscriber ever saw an error', seen.every(e => e === null), JSON.stringify(seen))
    off()
  }
  {
    // The service is down for good: every retry fails, and then it is reported.
    let down = true
    let loads = 0
    const poll = new SharedPoll(async () => {
      loads++
      return down ? { ok: false, error: 'TypeError: Failed to fetch', failure: 'unreachable' } : { ok: true, value: [{ id: 'a' }] }
    }, [], 100000, [5, 5])
    const off = poll.subscribe(() => {})
    await after(60)
    check('it retries once per delay, then stops', loads === 3, `loads=${loads}`)
    const error = poll.getSnapshot().error ?? ''
    check('and reports the failure once the retries fail', error.startsWith('no answer since ') && error.includes('still trying') && error.includes('Failed to fetch'), error)
    down = false
    await poll.refresh()
    check('an answer clears it', poll.getSnapshot().error === null)
    off()
  }
  {
    // A refusal is the service's answer; retrying will not change it.
    let loads = 0
    const poll = new SharedPoll(async () => { loads++; return { ok: false, error: 'HTTP 403', failure: 'refused' } }, [], 100000, [5])
    const off = poll.subscribe(() => {})
    await after(30)
    check('a refusal is reported at once', poll.getSnapshot().error === 'HTTP 403')
    check('and not retried', loads === 1, `loads=${loads}`)
    off()
  }
  {
    // Nobody watching: no retry fires after the last subscriber leaves.
    let loads = 0
    const poll = new SharedPoll(async () => { loads++; return { ok: false, error: 'x', failure: 'unreachable' } }, [], 100000, [10])
    const off = poll.subscribe(() => {})
    await settle()
    off()
    await after(30)
    check('a pending retry dies with the last subscriber', loads === 1, `loads=${loads}`)
  }

  console.log('SharedPoll — the registry')
  {
    const ownerA = () => {}
    const ownerB = () => {}
    const make = () => new SharedPoll(async () => ({ ok: true, value: [] }), [], 100000)
    const first = sharedPoll(ownerA, 'instances /api/bridge', make)
    check('the same fetch and URL get the same store', sharedPoll(ownerA, 'instances /api/bridge', make) === first)
    check('a different URL gets its own', sharedPoll(ownerA, 'machines /api/bridge', make) !== first)
    // Two providers can serve one basePath with different credentials; sharing
    // across them would serve one tenant's answer to the other.
    check('a different fetch gets its own', sharedPoll(ownerB, 'instances /api/bridge', make) !== first)
  }

  console.log('loadJSONList — the error wording the hooks have always used')
  {
    const ok = await loadJSONList(async () => ({ ok: true, json: async () => [{ id: 'a' }] }), '/u')
    check('a good response yields the list', ok.ok === true && ok.value.length === 1)

    const nullBody = await loadJSONList(async () => ({ ok: true, json: async () => null }), '/u')
    check('a null body reads as an empty list', nullBody.ok === true && nullBody.value.length === 0)

    const refused = await loadJSONList(async () => ({ ok: false, status: 500, json: async () => null }), '/u')
    check('a refused request reads "HTTP 500"', refused.ok === false && refused.error === 'HTTP 500', JSON.stringify(refused))

    const threw = await loadJSONList(async () => { throw new TypeError('offline') }, '/u')
    check('a thrown fetch keeps its own wording', threw.ok === false && threw.error === 'TypeError: offline', JSON.stringify(threw))
    check('and counts as unreachable', threw.failure === 'unreachable')
    check('a 500 counts as refused', refused.failure === 'refused')
    const gateway = await loadJSONList(async () => ({ ok: false, status: 502, headers: new Headers({ 'Content-Type': 'text/html' }), json: async () => null }), '/u')
    check("the web server's own 502 page counts as unreachable", gateway.ok === false && gateway.failure === 'unreachable')
    const peerDown = await loadJSONList(async () => ({ ok: false, status: 503, headers: new Headers({ 'Content-Type': 'application/json' }), json: async () => null }), '/u')
    check("the bridge's JSON 503 is its report of state, not retried", peerDown.ok === false && peerDown.failure === 'refused')
    const badBody = await loadJSONList(async () => ({ ok: true, json: async () => { throw new SyntaxError('bad json') } }), '/u')
    check('an unreadable body counts as refused', badBody.ok === false && badBody.failure === 'refused')
  }

  console.log('useBridgeHarnesses — the harness list, shared')
  {
    // The store is keyed on the URL, so the harnesses poll must not read the
    // answer the instances poll already put in the registry under the same fetch.
    const owner = () => {}
    const make = () => new SharedPoll(async () => ({ ok: true, value: [] }), [], 100000)
    const store = harnessesPoll(owner, '/api/bridge')
    check(
      'every caller of the harnesses hook gets one store',
      harnessesPoll(owner, '/api/bridge') === store,
    )
    check(
      'the hook does not read the instances answer',
      sharedPoll(owner, 'instances /api/bridge', make) !== store,
    )
    check(
      'the hook does not read the machines answer',
      sharedPoll(owner, 'machines /api/bridge', make) !== store,
    )
    check('a second basePath gets its own store', harnessesPoll(owner, '/api/other') !== store)
    // Two dashboards behind different credentials must not share an answer.
    check('a second provider gets its own store', harnessesPoll(() => {}, '/api/bridge') !== store)
  }
  {
    const list = [
      { name: 'claude_code', label: 'Claude Code', available: true },
      { name: 'codex', label: 'Codex', available: false },
    ]
    const map = harnessMapOf(list)
    check('the map is keyed on name', map.get('claude_code')?.label === 'Claude Code')
    check('an unregistered harness has no entry', map.get('nope') === undefined)
    check('an empty list yields an empty map', harnessMapOf([]).size === 0)
  }
  {
    // The settings form seeds editable state per harness. Polling means an
    // availability flip now arrives mid-edit; keyed on the name set, that tick
    // must not reseed the form and discard what the user typed.
    const before = [{ name: 'claude_code', available: false }, { name: 'codex', available: true }]
    const flipped = [{ name: 'claude_code', available: true }, { name: 'codex', available: true }]
    const added = [...flipped, { name: 'aider', available: true }]
    check('a harness coming available does not change the key', harnessNameKey(before) === harnessNameKey(flipped))
    check('a harness appearing does change the key', harnessNameKey(flipped) !== harnessNameKey(added))
    check('an empty list round-trips to no names', harnessNamesFromKey(harnessNameKey([])).length === 0)
    check('the key round-trips to the names it was built from',
      harnessNamesFromKey(harnessNameKey(added)).join() === 'claude_code,codex,aider')
    // A joined key would merge these two into one string and read the pair as
    // the single harness, hiding exactly the membership change this catches.
    check('two names cannot run together into one',
      harnessNameKey([{ name: 'a' }, { name: 'b' }]) !== harnessNameKey([{ name: 'a\nb' }]))
  }

  console.log('the sessions list token column')
  {
    // The column used to fetch every session's FULL message history and add
    // the usage up in the browser — 306MB and 52s for one long session, which
    // is why it was capped at 30 rows. It reads log-store's per-session
    // aggregate now. These pin the two halves that make one request enough:
    // when to ask, and that the answer settles.
    const rows = [
      { sessionId: 'a', state: 'idle' },
      { sessionId: 'b', state: 'running' },
      { sessionId: 'gone', state: 'idle' },
      { sessionId: 'never-ran', state: 'empty' },
    ]
    const aggregates = [
      { session_id: 'a', input_tokens: 5503, output_tokens: 8535 },
      {
        session_id: 'b', input_tokens: 7, output_tokens: 159,
        api_call_usage: [
          { query_source: 'sdk', calls: 2, input_tokens: 7, output_tokens: 159, cache_read_tokens: 90000, cache_write_tokens: 400 },
          { query_source: 'agent:builtin:general-purpose', calls: 30, input_tokens: 60, output_tokens: 5000, cache_read_tokens: 4000000, cache_write_tokens: 70000 },
        ],
      },
      { session_id: 'unrelated', input_tokens: 1, output_tokens: 1 },
    ]
    check('an empty map with rows on screen asks the server',
      sessionTokenTotalsAreMissing(rows, new Map()))
    check('a session that never took a turn is not worth asking about',
      !sessionTokenTotalsAreMissing([{ sessionId: 'never-ran', state: 'empty' }], new Map()))

    const settled = applySessionAggregates(new Map(), aggregates, rows)
    check('a row takes both totals from its aggregate',
      settled.get('a')?.mainConversation.input === 5503 && settled.get('a')?.mainConversation.output === 8535,
      JSON.stringify(settled.get('a')))
    // A session whose harness reports each model call is read from those
    // calls, so its cache tokens and its subagents' calls are in the row.
    check('a row with per-call usage keeps its subagents apart from its main conversation',
      settled.get('b')?.countedFrom === 'api-calls'
        && settled.get('b')?.mainConversation.cacheRead === 90000
        && settled.get('b')?.subagents.output === 5000
        && settled.get('b')?.subagents.cacheRead === 4000000
        && settled.get('b')?.subagentCalls === 30,
      JSON.stringify(settled.get('b')))
    // log-store omits sessions with no usage. Left absent they read as missing
    // forever, and the page re-fetches the whole aggregate on every render.
    check('a row the aggregate omits settles at zero',
      settled.get('gone')?.mainConversation.input === 0 && settled.get('gone')?.mainConversation.output === 0
        && settled.get('gone')?.subagents.output === 0,
      JSON.stringify(settled.get('gone')))
    check('one response answers every row on screen',
      !sessionTokenTotalsAreMissing(rows, settled))
    check('a session that never took a turn stays out of the map',
      !settled.has('never-ran'))
    // A row arriving after the fetch is the only thing that should ask again.
    check('a newly-appeared row asks again',
      sessionTokenTotalsAreMissing([...rows, { sessionId: 'new', state: 'idle' }], settled))
  }

  console.log('the components that used to fetch /harnesses themselves')
  {
    // These five components each owned an inline fetch of the list. They read
    // the shared store now, and nothing else here proves they are still wired
    // to it — a hook that returned an empty list would leave the pages looking
    // structurally fine and simply missing every harness.
    //
    // Rendering them against a populated store is what catches that. Only the
    // two that put harness markup on screen at rest are covered: BridgeInstances
    // hides its list behind an unopened form, and BridgeSessions wants a router.
    const harnesses = [
      { name: 'claude_code', label: 'Claude Code', image: '/images/harnesses/claude_code.png', available: true, capabilities: ['model', 'effort', 'budget', 'tools'] },
      { name: 'codex', label: 'Codex', image: null, available: false, capabilities: ['model'] },
    ]
    const basePath = '/api/bridge'
    const fetchFn = async url => ({
      ok: true,
      status: 200,
      json: async () => (url.endsWith('/harnesses') ? harnesses : []),
    })
    await harnessesPoll(fetchFn, basePath).refresh()
    const config = { fetch: fetchFn, basePath, routes: DEFAULT_BRIDGE_ROUTES }
    const render = Component =>
      renderToStaticMarkup(h(BridgeContext.Provider, { value: config }, h(Component)))

    const conformance = render(BridgeConformance)
    check('the conformance matrix has a row per harness',
      conformance.includes('Claude Code') && conformance.includes('Codex'), conformance.slice(0, 200))
    check('an empty matrix does not read as "No harnesses registered"',
      !conformance.includes('No harnesses registered'))

    const settings = render(BridgeSettings)
    check('the settings page has a card per harness',
      settings.includes('Claude Code') && settings.includes('Codex'), settings.slice(0, 200))
    // A HarnessInfo's `image` is server-relative, so a caller with only the map
    // and no basePath renders a broken logo.
    check('a harness logo resolves against basePath',
      settings.includes('/api/bridge/images/harnesses/claude_code.png'), settings.slice(0, 300))
    // Asserted on the badge class, not the word: the card's own
    // `bset-unavailable` modifier contains "unavailable" as a substring, so a
    // looser check passes with the badge deleted.
    check('a harness the server reports down is badged',
      settings.includes('bset-unavail-badge'))
    check('a harness the server reports up is not badged',
      settings.split('bset-unavail-badge').length - 1 === 1, settings.slice(0, 300))
  }
}
console.log('Composer auto-grow height')
{
  // The live measurement this pins, taken on https://dash.kayushkin.com/ with a
  // four-line draft: boxSizing border-box, border 1px/1px, offsetHeight 109,
  // clientHeight 107, scrollHeight 109. The old code assigned `scrollHeight`
  // straight, so the box was sized to 109 while only 107 of it could show
  // content — a scrollbar at every size, not just past the cap.
  const live = { scrollHeight: 109, boxSizing: 'border-box', borderTopWidth: '1px', borderBottomWidth: '1px' }

  check('under border-box the border is added back',
    composerAutoGrowHeightPx(live) === 111, String(composerAutoGrowHeightPx(live)))
  check('the assigned height leaves room for the content it was measured from',
    composerAutoGrowHeightPx(live) - 2 >= live.scrollHeight,
    `${composerAutoGrowHeightPx(live)} for scrollHeight ${live.scrollHeight}`)

  // Under content-box an assigned height already excludes the border, so adding
  // it overshoots by exactly as much as omitting it undershot.
  check('under content-box the height is the bare scrollHeight',
    composerAutoGrowHeightPx({ ...live, boxSizing: 'content-box' }) === 109,
    String(composerAutoGrowHeightPx({ ...live, boxSizing: 'content-box' })))

  // A borderless composer must not be padded by a phantom border, whichever
  // shape the used value comes back in.
  check('a zero border adds nothing',
    composerAutoGrowHeightPx({ ...live, borderTopWidth: '0px', borderBottomWidth: '0px' }) === 109)
  check('an unparseable border width adds nothing rather than NaN',
    composerAutoGrowHeightPx({ ...live, borderTopWidth: 'medium', borderBottomWidth: '' }) === 109,
    String(composerAutoGrowHeightPx({ ...live, borderTopWidth: 'medium', borderBottomWidth: '' })))

  // Asymmetric and fractional borders: the sum of the two edges is what the box
  // owes, not twice one of them, and a device-pixel-ratio border is not an integer.
  check('an asymmetric border sums both edges',
    composerAutoGrowHeightPx({ ...live, borderTopWidth: '3px', borderBottomWidth: '1px' }) === 113,
    String(composerAutoGrowHeightPx({ ...live, borderTopWidth: '3px', borderBottomWidth: '1px' })))
  check('a fractional border is not rounded away',
    composerAutoGrowHeightPx({ ...live, borderTopWidth: '0.5px', borderBottomWidth: '0.5px' }) === 110,
    String(composerAutoGrowHeightPx({ ...live, borderTopWidth: '0.5px', borderBottomWidth: '0.5px' })))

  // The cap belongs to `.bc-composer-input { max-height: 220px }` in this
  // package's stylesheet. A tall draft must come back TALLER than the cap, so the
  // browser is the thing that clamps it; the moment this function starts
  // returning 220 it has grown a second copy of that number to keep in step.
  check('a draft past the cap is not clamped here — the stylesheet clamps it',
    composerAutoGrowHeightPx({ ...live, scrollHeight: 600 }) === 602,
    String(composerAutoGrowHeightPx({ ...live, scrollHeight: 600 })))
}

// BridgePrefsStore — the bridge-prefs record, held once per endpoint. Driven
// directly rather than through React for the same reason as SharedPoll: what
// these pin is how many copies of the record exist and what a second consumer
// sees when the first one writes, and neither is visible in rendered markup.
async function bridgePrefsChecks() {
  const settle = () => new Promise(resolve => setTimeout(resolve, 0))

  // localStorage exists in a browser, not in node. The store's own writes are
  // wrapped so its absence is survivable, but the checks that assert what gets
  // persisted need somewhere for it to land.
  const localStorageBacking = new Map()
  globalThis.localStorage = {
    getItem: key => (localStorageBacking.has(key) ? localStorageBacking.get(key) : null),
    setItem: (key, value) => { localStorageBacking.set(key, String(value)) },
    removeItem: key => { localStorageBacking.delete(key) },
  }

  console.log('\nmergePrefs — a partial folded onto the record')
  {
    const prev = {
      last_instance_id: 'inst-1',
      last_session: { cc: 's1', codex: 's2' },
      defaults: { cc: { model: 'sonnet', max_budget: 5 }, codex: { model: 'gpt' } },
    }
    const next = mergePrefs(prev, { defaults: { cc: { model: 'opus' } } })
    check('writing one harness keeps the others', next.defaults.codex?.model === 'gpt', JSON.stringify(next))
    // Replace-to-clear: the settings editor deletes a field by writing the
    // whole record without it. Merging inside the store would make that
    // impossible, so the merge that belongs to a caller stays at the caller.
    check('a harness record is replaced whole, not merged',
      next.defaults.cc?.max_budget === undefined && next.defaults.cc?.model === 'opus', JSON.stringify(next.defaults.cc))
    check('an untouched field survives', next.last_instance_id === 'inst-1')
    check('last_session merges by key',
      mergePrefs(prev, { last_session: { cc: 's9' } }).last_session?.codex === 's2')
    check('the input is not mutated', prev.defaults.cc.model === 'sonnet')
  }

  console.log('BridgePrefsStore — one record, many consumers')
  {
    let gets = 0
    let put = null
    const fetchFn = async (url, init) => {
      if (init?.method === 'PUT') { put = JSON.parse(init.body); return { ok: true, json: async () => ({}) } }
      gets++
      // `last_instance_id` is here so the merged record differs from any one
      // partial. Without a second field the two are identical and every check
      // that says "the partial, not the record" passes without asserting.
      return { ok: true, json: async () => ({ last_instance_id: 'inst-1', defaults: { cc: { model: 'sonnet', max_budget: 5 } } }) }
    }
    const store = bridgePrefsStoreFor({ fetch: fetchFn, endpoint: '/api/bridge/bridge-prefs', storagePrefix: 'bridge-prefs' })
    check('nobody subscribed means no request', gets === 0)

    let notifiedA = 0
    let notifiedB = 0
    const offA = store.subscribe(() => { notifiedA++ })
    const offB = store.subscribe(() => { notifiedB++ })
    await settle()
    // The defect this whole file exists to close: two consumers used to load
    // the record separately and then disagree the moment either wrote.
    check('two subscribers cost one GET', gets === 1, `gets=${gets}`)
    check('both were told when it arrived', notifiedA === 1 && notifiedB === 1, `${notifiedA}/${notifiedB}`)
    check('and both read the same object', store.getSnapshot().prefs.defaults?.cc?.model === 'sonnet')
    check('loaded flips once the record is in', store.getSnapshot().loaded === true)

    // A write by one consumer is the other consumer's new record. Before the
    // store, this is exactly where a stale copy resurrected a cleared ceiling.
    await store.update({ defaults: { cc: { model: 'opus' } } })
    check('a write reaches every subscriber', notifiedA === 2 && notifiedB === 2, `${notifiedA}/${notifiedB}`)
    check('the record is the merged one', store.getSnapshot().prefs.defaults?.cc?.model === 'opus')
    check('and the fields the write did not name are still there',
      store.getSnapshot().prefs.last_instance_id === 'inst-1')
    check('the PUT body is the partial, not the whole record',
      JSON.stringify(put) === JSON.stringify({ defaults: { cc: { model: 'opus' } } }), JSON.stringify(put))
    check('localStorage gets the merged record',
      JSON.parse(localStorageBacking.get('bridge-prefs')).defaults.cc.model === 'opus')

    offA()
    offB()
    check('the record survives its last subscriber leaving',
      store.getSnapshot().prefs.defaults?.cc?.model === 'opus' && store.getSnapshot().loaded === true)
    const back = store.subscribe(() => {})
    await settle()
    check('and a consumer that comes back costs no second GET', gets === 1, `gets=${gets}`)
    back()
  }

  console.log('BridgePrefsStore — a write that races the first load')
  {
    let release
    const held = new Promise(resolve => { release = resolve })
    const fetchFn = async (_url, init) => {
      if (init?.method === 'PUT') return { ok: true, json: async () => ({}) }
      await held
      return { ok: true, json: async () => ({ defaults: { cc: { model: 'sonnet', max_budget: 5 } } }) }
    }
    const store = bridgePrefsStoreFor({ fetch: fetchFn, endpoint: '/api/bridge/bridge-prefs', storagePrefix: 'bridge-prefs' })
    const off = store.subscribe(() => {})
    // The user clears the ceiling before the GET has come back. Publishing the
    // server's record wholesale on arrival would put the ceiling straight back.
    await store.update({ defaults: { cc: { model: 'sonnet' } } })
    release()
    await settle()
    await settle()
    check('the write survives the load that lands after it',
      store.getSnapshot().prefs.defaults.cc.max_budget === undefined,
      JSON.stringify(store.getSnapshot().prefs.defaults))
    check('and the loaded record is still underneath it',
      store.getSnapshot().prefs.defaults.cc.model === 'sonnet')
    off()
  }

  console.log('BridgePrefsStore — a read that fails')
  {
    const store = bridgePrefsStoreFor({
      fetch: async () => { throw new TypeError('offline') },
      endpoint: '/api/bridge/bridge-prefs',
      storagePrefix: 'bridge-prefs',
    })
    const off = store.subscribe(() => {})
    await settle()
    // A consumer gated on `loaded` — the chat's bootstrap is — must not wait
    // forever because the record could not be read.
    check('a failed read still settles', store.getSnapshot().loaded === true)
    check('and reports an empty record, not a broken one',
      JSON.stringify(store.getSnapshot().prefs) === '{}', JSON.stringify(store.getSnapshot().prefs))
    off()
  }

  console.log('BridgePrefsStore — localStorage-only mode')
  {
    localStorageBacking.set('other-prefs', JSON.stringify({ last_harness: 'codex' }))
    const store = bridgePrefsStoreFor({ storagePrefix: 'other-prefs' })
    const off = store.subscribe(() => {})
    await settle()
    check('a store with no server reads localStorage', store.getSnapshot().prefs.last_harness === 'codex')
    await store.update({ last_instance_id: 'inst-9' })
    check('and writes the merged record back',
      JSON.parse(localStorageBacking.get('other-prefs')).last_instance_id === 'inst-9')
    check('without disturbing what was already there',
      JSON.parse(localStorageBacking.get('other-prefs')).last_harness === 'codex')
    off()
  }

  console.log('BridgePrefsStore — the registry')
  {
    const fetchA = async () => ({ ok: true, json: async () => ({}) })
    const fetchB = async () => ({ ok: true, json: async () => ({}) })
    const key = { fetch: fetchA, endpoint: '/api/bridge/bridge-prefs', storagePrefix: 'bridge-prefs' }
    const first = bridgePrefsStoreFor(key)
    check('the same fetch and endpoint get the same store', bridgePrefsStoreFor({ ...key }) === first)
    check('a different endpoint gets its own',
      bridgePrefsStoreFor({ ...key, endpoint: '/other/bridge-prefs' }) !== first)
    // Two providers can serve one basePath with different credentials; sharing
    // across them would hand one tenant's saved defaults to the other.
    check('a different fetch gets its own', bridgePrefsStoreFor({ ...key, fetch: fetchB }) !== first)
    // The prefs store and a poll can be asked for under the same fetch. They
    // share one registry, so their keys must not collide.
    const poll = sharedPoll(fetchA, 'instances /api/bridge', () =>
      new SharedPoll(async () => ({ ok: true, value: [] }), [], 100000))
    check('a poll under the same fetch is a different object', poll !== first)
  }

  // `permission_mode` is written through `POST /bridge/permission-mode`, not
  // through `PUT /bridge-prefs`, so its writer has to tell the store to read
  // again. What the checks below pin is that doing so cannot cost anything the
  // page was already holding.
  console.log('\nreconcilePrefs — a re-read that changed nothing changed nothing')
  {
    const prev = {
      last_harness: 'cc',
      last_session: { cc: 's1' },
      defaults: { cc: { model: 'sonnet', max_budget: 5, disabled_tools: ['Bash'] } },
    }
    const same = JSON.parse(JSON.stringify(prev))
    check('an equal record is the SAME object, not an equal one',
      reconcilePrefs(prev, same) === prev)
    // The one that matters. `useBridgePrefs` re-creates `getDefaults` whenever
    // `prefs.defaults` changes identity, and the settings form seeds itself in
    // an effect keyed on that — so a re-read that rebuilt `defaults` from JSON
    // would wipe a half-typed spend ceiling every time the mode was changed.
    const modeOnly = reconcilePrefs(prev, { ...same, permission_mode: 'bypass' })
    check('a change elsewhere leaves defaults identical by reference',
      modeOnly.defaults === prev.defaults, 'defaults was rebuilt')
    check('and last_session too', modeOnly.last_session === prev.last_session)
    check('while the changed field is the new one', modeOnly.permission_mode === 'bypass')
    check('the record itself is new when something changed', modeOnly !== prev)
    // Structural sharing must not become "never notice a change".
    const changed = reconcilePrefs(prev, { ...same, defaults: { cc: { model: 'opus' } } })
    check('a real change to defaults IS a new object', changed.defaults !== prev.defaults)
    check('and carries the new value', changed.defaults?.cc?.model === 'opus')
    check('a dropped field counts as a change',
      reconcilePrefs(prev, { last_session: { cc: 's1' } }) !== prev)
    check('an array that differs in order counts as a change',
      reconcilePrefs({ defaults: { cc: { disabled_tools: ['Bash', 'Read'] } } },
        { defaults: { cc: { disabled_tools: ['Read', 'Bash'] } } }).defaults?.cc?.disabled_tools?.[0] === 'Read')
  }

  console.log('BridgePrefsStore — refresh, for the writer that uses another endpoint')
  {
    let gets = 0
    let served = { permission_mode: 'ask', defaults: { cc: { model: 'sonnet' } } }
    const fetchFn = async (_url, init) => {
      if (init?.method === 'PUT') return { ok: true, json: async () => ({}) }
      gets++
      return { ok: true, json: async () => JSON.parse(JSON.stringify(served)) }
    }
    const store = bridgePrefsStoreFor({
      fetch: fetchFn, endpoint: '/api/bridge/refresh-prefs', storagePrefix: 'refresh-prefs',
    })
    let notified = 0
    const off = store.subscribe(() => { notified++ })
    await settle()
    check('the record loaded once', gets === 1 && store.getSnapshot().prefs.permission_mode === 'ask')
    const defaultsBefore = store.getSnapshot().prefs.defaults

    // The selector POSTs its mode elsewhere and then asks for a re-read.
    served = { permission_mode: 'bypass', defaults: { cc: { model: 'sonnet' } } }
    await store.refresh()
    check('a refresh re-reads the record', gets === 2, `gets=${gets}`)
    check('and publishes the field the other endpoint wrote',
      store.getSnapshot().prefs.permission_mode === 'bypass')
    check('every subscriber hears about it', notified === 2, `notified=${notified}`)
    check('but the unchanged half keeps its identity',
      store.getSnapshot().prefs.defaults === defaultsBefore, 'defaults was rebuilt')

    // A refresh is not free of consequence only when it finds something. When
    // it does not, a subscriber must not be woken at all — waking the settings
    // form is how unsaved edits get thrown away.
    const quiet = notified
    await store.refresh()
    check('a refresh that finds nothing new notifies nobody',
      notified === quiet, `notified went ${quiet} -> ${notified}`)
    check('and it still cost a request, because only the server knows', gets === 3, `gets=${gets}`)
    off()
  }

  console.log('BridgePrefsStore — a write that races a refresh')
  {
    let release
    let held = null
    const fetchFn = async (_url, init) => {
      if (init?.method === 'PUT') return { ok: true, json: async () => ({}) }
      if (held) await held
      return { ok: true, json: async () => ({ defaults: { cc: { model: 'sonnet', max_budget: 5 } } }) }
    }
    const store = bridgePrefsStoreFor({
      fetch: fetchFn, endpoint: '/api/bridge/race-prefs', storagePrefix: 'race-prefs',
    })
    const off = store.subscribe(() => {})
    await settle()
    check('the ceiling is there to start with',
      store.getSnapshot().prefs.defaults?.cc?.max_budget === 5)

    held = new Promise(resolve => { release = resolve })
    const refreshing = store.refresh()
    // The user clears the ceiling while the re-read is in flight. The server's
    // answer predates the clear, so publishing it wholesale puts the ceiling
    // back — the same defect the initial load already guards against, which is
    // why both go through one code path.
    await store.update({ defaults: { cc: { model: 'sonnet' } } })
    release()
    await refreshing
    await settle()
    check('a write made during a refresh survives it',
      store.getSnapshot().prefs.defaults?.cc?.max_budget === undefined,
      JSON.stringify(store.getSnapshot().prefs.defaults))
    off()
  }
}

console.log('\nSplit drag geometry — one implementation for both splits')
{
  const near = (a, b) => Math.abs(a - b) < 1e-9
  // A 1000px pair split evenly: one grow unit is worth 500px.
  const even = measureSplitDragGeometry(1000, 1, 1)
  check('an even 1000px pair measures 500px per grow unit',
    near(even.pixelsPerGrowUnit, 500), JSON.stringify(even))
  check('180px of minimum is 0.36 grow units at that scale',
    near(even.minimumGrowUnits, MINIMUM_PANE_PIXELS / 500), JSON.stringify(even))

  const still = splitGrowUnitsAfterDrag(even, 0)
  check('a drag of zero pixels changes nothing',
    near(still.growUnitsBefore, 1) && near(still.growUnitsAfter, 1), JSON.stringify(still))

  const moved = splitGrowUnitsAfterDrag(even, 250)
  check('dragging 250px moves half a grow unit across the boundary',
    near(moved.growUnitsBefore, 1.5) && near(moved.growUnitsAfter, 0.5), JSON.stringify(moved))
  check('the pair total is conserved by a drag',
    near(moved.growUnitsBefore + moved.growUnitsAfter, even.totalGrowUnits), JSON.stringify(moved))

  // Past the minimum the boundary stops rather than inverting, and the far side
  // takes exactly the remainder — a clamp that moved only one side would leak grow.
  const pinnedLeft = splitGrowUnitsAfterDrag(even, -5000)
  check('dragging past the minimum pins the near pane at the minimum',
    near(pinnedLeft.growUnitsBefore, even.minimumGrowUnits), JSON.stringify(pinnedLeft))
  check('the pinned pair still sums to the total',
    near(pinnedLeft.growUnitsBefore + pinnedLeft.growUnitsAfter, even.totalGrowUnits),
    JSON.stringify(pinnedLeft))
  const pinnedRight = splitGrowUnitsAfterDrag(even, 5000)
  check('the clamp is symmetric',
    near(pinnedRight.growUnitsAfter, even.minimumGrowUnits)
    && near(pinnedRight.growUnitsBefore + pinnedRight.growUnitsAfter, even.totalGrowUnits),
    JSON.stringify(pinnedRight))

  // A pair too narrow to give both sides 180px must split evenly, not hand one
  // side 180 and the other a negative.
  const cramped = measureSplitDragGeometry(200, 1, 1)
  check('a pair narrower than two minimums caps the minimum at half the pair',
    near(cramped.minimumGrowUnits, cramped.totalGrowUnits / 2), JSON.stringify(cramped))
  const crampedDrag = splitGrowUnitsAfterDrag(cramped, -5000)
  check('a cramped pair clamps to an even split rather than a negative',
    near(crampedDrag.growUnitsBefore, 1) && near(crampedDrag.growUnitsAfter, 1),
    JSON.stringify(crampedDrag))

  // An unmeasurable pair has no scale to convert pixels with. Returning null is
  // what makes the handle a no-op instead of writing NaN into the layout.
  check('a pair with no extent on screen cannot be measured',
    measureSplitDragGeometry(0, 1, 1) === null)
  check('a pair with no grow between them cannot be measured',
    measureSplitDragGeometry(1000, 0, 0) === null)

  // Grow units are a ratio, so an asymmetric pair scales the same way.
  const lopsided = measureSplitDragGeometry(900, 2, 1)
  check('an asymmetric pair measures per grow unit, not per pane',
    near(lopsided.pixelsPerGrowUnit, 300), JSON.stringify(lopsided))
  const lopsidedDrag = splitGrowUnitsAfterDrag(lopsided, -300)
  check('an asymmetric pair moves one grow unit per 300px',
    near(lopsidedDrag.growUnitsBefore, 1) && near(lopsidedDrag.growUnitsAfter, 2),
    JSON.stringify(lopsidedDrag))

  check('the double-click reset is an even split',
    EVEN_SPLIT_GROW_UNITS.growUnitsBefore === 1 && EVEN_SPLIT_GROW_UNITS.growUnitsAfter === 1)

  // The copy this replaced could only do a horizontal split, so the axis being a
  // real parameter is the point of the merge, not a detail of it.
  const noPair = () => null
  const horizontal = renderToStaticMarkup(h(SplitDragHandle, {
    axis: 'horizontal', className: 'bc-split-resizer',
    resolveDraggedPair: noPair, commitGrowUnits: () => {},
  }))
  const vertical = renderToStaticMarkup(h(SplitDragHandle, {
    axis: 'vertical', className: 'bc-workspace-resizer bc-workspace-resizer-v',
    resolveDraggedPair: noPair, commitGrowUnits: () => {},
  }))
  check('a separator between side-by-side panes is a vertical line',
    horizontal.includes('aria-orientation="vertical"'), horizontal)
  check('a separator between stacked panes is a horizontal line',
    vertical.includes('aria-orientation="horizontal"'), vertical)
  check('the handle keeps each split\'s own class, so neither style moved',
    horizontal.includes('class="bc-split-resizer"')
    && vertical.includes('class="bc-workspace-resizer bc-workspace-resizer-v"'),
    `${horizontal} ${vertical}`)
  check('both axes announce themselves as a separator',
    horizontal.includes('role="separator"') && vertical.includes('role="separator"'))
}

console.log('BridgeLayout — a narrow viewport is not permission to hide the navigation')
{
  // Below 640px `minimal` goes true on EVERY page mounted under a `BridgeProvider`,
  // because the provider rides along inside it. Only the chat answers that by
  // drawing a replacement top bar and drawer. These checks pin the unanswered case:
  // the tab row is the only navigation the other twelve pages have, and the host's
  // header is hidden by the same signal, so dropping it strands the user.
  //
  // The answered case — the chat, where the nav SHOULD go — cannot be checked here:
  // registration is a layout effect and `renderToStaticMarkup` runs no effects. It
  // is covered in a browser by dash's `e2e/minimal-chrome-navigation.spec.ts`.
  const realWindow = globalThis.window
  globalThis.window = {
    innerWidth: 600,
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    addEventListener: () => {},
    removeEventListener: () => {},
  }
  let narrow
  try {
    narrow = renderToStaticMarkup(
      h(MemoryRouter, { initialEntries: ['/instances'] },
        h(BridgeContext.Provider, { value: { fetch: async () => ({ ok: true, status: 200, json: async () => [] }), basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES } },
          h(MinimalChromeProvider, null, h(BridgeLayout)))))
  } finally {
    if (realWindow === undefined) delete globalThis.window
    else globalThis.window = realWindow
  }

  check('a 600px viewport with no minimal chrome drawn keeps the tab nav',
    narrow.includes('bridge-nav'), narrow.slice(0, 300))
  // Two rows since 2026-09-18: the groups, then the pages of the group the path
  // belongs to. /instances is in Agents, so its row is Instances, Agents, Files.
  check('every group and the current group\'s tabs are still reachable, not just the element',
    ['Work', 'Agents', 'Access', 'System', 'Instances', 'Files'].every(t => narrow.includes(t)),
    narrow.slice(0, 300))
  check('the other groups\' pages are one click away, not on this row',
    !narrow.includes('>Sessions<') && narrow.includes('href="/"'),
    narrow.slice(0, 300))
  // The class strips the content padding and takes the full height for a chat
  // that has taken the screen over. Applying it to a page that did not is the
  // same mistake wearing different clothes.
  check('bridge-layout-minimal is not applied when no chrome was drawn',
    !narrow.includes('bridge-layout-minimal'), narrow.slice(0, 300))

  // The no-provider fallback has to fail the same way — a host that mounts a
  // component outside the provider must not be told a chrome exists.
  check('the fallback context reports no minimal chrome mounted',
    useMinimalChromeFallbackReportsNoChrome())
}

// --- the side panels a host composes itself ---------------------------------
//
// These three render through `../src/index.ts` on purpose. Importing them from
// their own modules would prove they render but not that they are *reachable*,
// and reachability is the whole point: a host that builds its own layout out of
// this library's parts (dash's chat page does) can only mount what the index exports.
//
// They are also mounted with nothing above them but `BridgeProvider`'s context
// and a router — no `WorkspaceProvider`. That is the real deployment shape, and
// it is the condition `GitPanel` used to fail: it read the workspace context
// directly, and `useWorkspace` throws rather than returning a default, so the
// export alone would have handed a host a component that dies on first render.
// The principal directory's two rules, which pull in opposite directions and
// are both load-bearing: a disabled principal RESOLVES (a card assigned to
// someone who left still names them) and is NOT PICKABLE (nobody assigns new
// work to them, and kanban-store would 400 the attempt anyway).
console.log('\nprincipals — disabled resolves but is not pickable')
{
  const slava = { id: 'principal_000001', kind: 'human', display_name: 'Slava Kayushkin', email: 'v@example.com', disabled_at: 0, created_at: 1, updated_at: 1 }
  const gone = { id: 'principal_000002', kind: 'human', display_name: 'Gone Person', email: '', disabled_at: 1_700_000_000, created_at: 1, updated_at: 1 }
  const team = { id: 'principal_000003', kind: 'group', display_name: 'Platform Team', email: '', disabled_at: 0, created_at: 1, updated_at: 1 }
  const list = [slava, gone, team]
  const byId = indexPrincipalsByID(list)
  check('a disabled principal still resolves by id', byId.get('principal_000002') === gone)
  check('and is reported disabled', principalIsDisabled(gone) && !principalIsDisabled(slava))
  const all = pickablePrincipals(list, { query: '', kind: 'all', excludeIDs: [] })
  check('the picker never offers a disabled principal', !all.includes(gone) && all.length === 2, JSON.stringify(all.map(p => p.id)))
  check('the query is a case-insensitive substring of display_name',
    pickablePrincipals(list, { query: 'KAYUSH', kind: 'all', excludeIDs: [] }).map(p => p.id).join() === 'principal_000001')
  check('an already-assigned principal is not offered again',
    pickablePrincipals(list, { query: '', kind: 'all', excludeIDs: ['principal_000001'] }).map(p => p.id).join() === 'principal_000003')
  check('the kind toggle narrows to people', pickablePrincipals(list, { query: '', kind: 'human', excludeIDs: [] }).map(p => p.id).join() === 'principal_000001')
  check('the kind toggle narrows to groups', pickablePrincipals(list, { query: '', kind: 'group', excludeIDs: [] }).map(p => p.id).join() === 'principal_000003')
  check('initials are the first letters of the first two words', principalInitials('Slava Kayushkin') === 'SK' && principalInitials('priya') === 'P' && principalInitials('  ') === '?')
}

// The drawer's "Assigned" section, rendered with no directory answer yet. A
// static render never runs the fetch, so the directory is in exactly the state
// it is in when principal-store is down or slow — and that is the case the
// chips must survive: the raw id is on screen, never nothing.
console.log('\nCardDetail — assignees render honestly without a directory')
{
  const TS = '2026-08-16T05:00:00Z'
  const card = (assignments, boardID = 'b1') => ({
    placement: { card_id: 'c1', board_id: boardID, column_id: 'col1', position: 0, created_at: TS, updated_at: TS },
    item: { id: 'c1', type: 'todo', title: 'A card', body: '', tags: [], status: 'open', created_at: TS, updated_at: TS },
    links: [],
    assignments,
  })
  const noop = async () => true
  const outcome = async () => ({ ok: true })
  const mount = (principalsBasePath, assignments, boardID = 'b1') => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/kanban'] },
      h(BridgeContext.Provider, { value: {
        fetch: async () => ({ ok: true, status: 200, text: async () => '', json: async () => [] }),
        basePath: '/api/bridge', issuesBasePath: '/api/kanban', principalsBasePath,
        noteboardBasePath: '', mailBasePath: '', mailPagePath: '', routes: DEFAULT_BRIDGE_ROUTES,
      } },
        h(CardDetail, {
          card: card(assignments, boardID), boardID, entityTypes: [{ type: 'session' }],
          onClose: () => {}, onPatch: noop, onDetach: () => {}, onArchive: () => {}, onDelete: () => {},
          onAddLink: noop, onDeleteLink: noop, onAssign: outcome, onUnassign: outcome,
          onOpenChat: () => {}, onOpenInMail: () => {}, mailBasePath: '', fetchFn: async () => ({ ok: true }),
        }))))
  const two = mount('/api/principals', [
    { card_id: 'c1', principal_id: 'principal_000001', assigned_by: 'me', created_at: TS },
    { card_id: 'c1', principal_id: 'principal_000002', assigned_by: 'me', created_at: TS },
  ])
  check('the section is present when a principal-store path is configured', two.includes('>Assigned<'), two)
  check('an unresolved assignee shows its raw id rather than nothing',
    two.includes('principal_000001') && two.includes('principal_000002') && two.includes('bk-assignee-unresolved'))
  check('the unresolved chip explains why on hover', two.includes('principals are still loading'))
  const none = mount('', [{ card_id: 'c1', principal_id: 'principal_000001', assigned_by: 'me', created_at: TS }])
  check('no principal-store path hides the whole section', !none.includes('>Assigned<') && !none.includes('bk-assignee'), none.slice(0, 400))
  const unknown = mount('/api/principals', undefined)
  check('a card whose assignments were never loaded says so, not "nobody"',
    unknown.includes('not loaded on this view') && !unknown.includes('Nobody yet') && !unknown.includes('bk-assignee-picker'))
  // Where the agent runs is a choice on the drawer, and a card with no session
  // has nothing chosen: the start button waits for one rather than guessing.
  check('the drawer offers a "Runs on" select', two.includes('bk-agent-target-select') && two.includes('Choose a harness instance'), two.slice(0, 1200))
  check('with nothing chosen, the start button is disabled and says why',
    /<button[^>]*disabled=""[^>]*title="Choose where the agent runs first"[^>]*>▶ Start an agent on this/.test(two))
  // A card on a board waits for kanban-store to resolve its defaults, and a
  // static render never gets that answer — so the note must claim nothing yet.
  check('while the card’s defaults are still being read, no note claims where it runs', !two.includes('bk-agent-target-note'), two.slice(0, 1200))
  // A card on no board has no defaults to wait for, so the note is settled on
  // the first render.
  const offBoard = mount('/api/principals', [], '')
  check('a card on no board says it has had no session yet',
    offBoard.includes('<span class="bk-agent-target-note">this card has had no session yet</span>'), offBoard.slice(0, 1200))
}

// The Principals page — the editor for the directory the assignee chips
// resolve against. Static renders cannot run its fetches, so these pin what
// can be pinned without one: the request it would make, what its views do with
// a given answer, and that an unconfigured host gets nothing at all.
console.log('\nBridgePrincipals — the directory editor')
{
  const bridgeConfig = (principalsBasePath) => ({
    fetch: async () => ({ ok: true, status: 200, text: async () => '', json: async () => [] }),
    basePath: '/api/bridge', issuesBasePath: '/api/kanban', principalsBasePath,
    noteboardBasePath: '', mailBasePath: '', mailPagePath: '', routes: DEFAULT_BRIDGE_ROUTES,
  })
  const mountPage = (principalsBasePath) => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/principals'] },
      h(BridgeContext.Provider, { value: bridgeConfig(principalsBasePath) }, h(BridgePrincipals))))

  check('no principal-store path says it is not configured', mountPage('').includes('principal-store is not configured'), JSON.stringify(mountPage('')))
  const page = mountPage('/api/principals')
  check('a configured host gets the page', page.includes('bp-container') && page.includes('>Principals<'), page.slice(0, 300))
  check('the kind filter offers All / People / Groups', ['>All<', '>People<', '>Groups<'].every(t => page.includes(t)), page.slice(0, 600))
  check('the disabled toggle is off by default', page.includes('Show disabled') && !page.includes('checked=""'), page.slice(0, 900))

  // The listing request. Disabled principals are hidden unless the toggle is
  // on, and that is the SERVER's rule (`include_disabled`), so the honest
  // check is on the URL the page asks for, not on a client-side filter.
  const base = '/api/principals'
  const url = (search) => principalsSearchURL(base, { query: '', kind: 'all', includeDisabled: false, limit: 500, ...search })
  check('the default listing does not ask for disabled principals', !url({}).includes('include_disabled'), url({}))
  check('the toggle asks for them', url({ includeDisabled: true }).includes('include_disabled=true'), url({ includeDisabled: true }))
  check('the kind filter is a server parameter', url({ kind: 'group' }).includes('kind=group') && !url({ kind: 'all' }).includes('kind='), url({ kind: 'group' }))
  check('the search box is the store\'s prefix search', url({ query: '  pri ' }).includes('q=pri'), url({ query: '  pri ' }))
  check('an empty query sends no q', !url({ query: '' }).includes('q='), url({}))

  const TS = 1_700_000_000
  const slava = { id: 'principal_000001', kind: 'human', display_name: 'Slava Kayushkin', email: 'v@example.com', disabled_at: 0, created_at: TS, updated_at: TS }
  const gone = { id: 'principal_000002', kind: 'human', display_name: 'Gone Person', email: '', disabled_at: TS, created_at: TS, updated_at: TS }
  const team = { id: 'principal_000003', kind: 'group', display_name: 'Platform Team', email: '', disabled_at: 0, created_at: TS, updated_at: TS }

  // The roster renders what the server sent and flags the disabled row; it
  // never drops one on its own, or the toggle would lie.
  const roster = renderToStaticMarkup(h(PrincipalListView, {
    principals: [slava, gone, team], selectedID: 'principal_000003', onSelect: () => {}, loading: false, error: null,
  }))
  check('every row the server sent is listed', ['principal_000001', 'principal_000002', 'principal_000003'].every(id => roster.includes(`data-principal-id="${id}"`)), roster.slice(0, 400))
  check('a disabled row carries the badge, an active one does not',
    roster.split('data-principal-id="principal_000002"')[1].split('</li>')[0].includes('bp-badge-disabled')
      && !roster.split('data-principal-id="principal_000001"')[1].split('</li>')[0].includes('bp-badge-disabled'))
  check('a person is an initials avatar, a group the group glyph', roster.includes('>SK<') && roster.includes('👥'), roster.slice(0, 400))
  check('the selected row is marked', roster.includes('bp-row-selected') && roster.includes('aria-pressed="true"'))
  const failed = renderToStaticMarkup(h(PrincipalListView, {
    principals: [slava], selectedID: null, onSelect: () => {}, loading: false, error: 'HTTP 502',
  }))
  check('a failed listing shows the failure and keeps the last rows', failed.includes('HTTP 502') && failed.includes('principal_000001'), failed)

  // The detail view: the id in monospace for pasting, and the state said plainly.
  const outcome = async () => ({ ok: true, value: slava })
  const never = () => new Promise(() => {})
  const detail = (row) => renderToStaticMarkup(h(PrincipalDetailView, {
    detail: row, loading: false, readError: null,
    save: outcome, setDisabled: outcome, addMembership: outcome, removeMembership: outcome,
    searchCandidates: never, onChanged: async () => {}, onOpen: () => {},
  }))
  const human = detail({ ...slava, groups: [team] })
  check('the id is shown as monospace text', human.includes('<code class="bp-id"') && human.includes('>principal_000001</code>'), human.slice(0, 500))
  check('an active principal offers Disable, and says Active', human.includes('>Disable<') && human.includes('>Active<') && !human.includes('>Enable<'))
  check('a person has an email field', human.includes('>Email<'))
  check('a human lists its groups with a remove control', human.includes('>Groups <') && human.includes('Platform Team') && human.includes('Remove Platform Team'))
  check('and its picker offers groups', human.includes('data-picker-kind="group"') && human.includes('Add to a group'))
  const group = detail({ ...team, members: [slava, gone] })
  check('a group has no email field', !group.includes('>Email<'), group.slice(0, 800))
  check('a group lists its members, a disabled one flagged', group.includes('>Members <') && group.includes('Gone Person')
    && group.split('data-principal-id="principal_000002"')[1].split('</li>')[0].includes('bp-badge-disabled'))
  check('and its picker offers people', group.includes('data-picker-kind="human"') && group.includes('Add a person'))
  const disabledRow = detail({ ...gone, groups: [] })
  check('a disabled principal offers Enable, and says since when', disabledRow.includes('>Enable<') && disabledRow.includes('Disabled since') && !disabledRow.includes('>Disable<'))

  // A refusal is shown in the server's words. The nested-group 400 is the one
  // this page is most likely to meet.
  const refusal = "nested groups are not supported in v1: add the group's humans directly"
  const refused = renderToStaticMarkup(h(MembershipsSection, {
    title: 'Members', emptyText: 'No members yet.', entries: [slava], pickerKind: 'human', pickerPlaceholder: 'Add a person…',
    selfID: 'principal_000003', add: outcome, remove: outcome, searchCandidates: never, onChanged: async () => {}, onOpen: () => {},
    initialError: refusal,
  }))
  check('a group-as-member refusal is shown verbatim', refused.includes('bp-membership-error') && refused.includes('nested groups are not supported in v1: add the group&#x27;s humans directly'), refused)

  // The tab follows the base path, exactly as the Kanban tab does.
  // Principals sits in the Access group, so the row that would show it is the
  // one drawn at its own path.
  const layout = (principalsBasePath) => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/principals'] },
      h(BridgeContext.Provider, { value: bridgeConfig(principalsBasePath) },
        h(MinimalChromeProvider, null, h(BridgeLayout)))))
  check('BridgeLayout shows a Principals tab when principal-store is configured',
    layout('/api/principals').includes('>Principals<') && layout('/api/principals').includes('href="/principals"'))
  check('and none when it is not', !layout('').includes('Principals'))
}

// The Bundles page, moved here from dash on 2026-09-10. Static renders cannot
// run its fetches, so these pin the gates and what a bundle card shows.
console.log('\nBridgeBundles — bundle-store bundles and the resolve preview')
{
  const bundlesConfig = (bundlesBasePath, reposBasePath = '') => ({
    fetch: async () => ({ ok: true, status: 200, text: async () => '', json: async () => [] }),
    basePath: '/api/bridge', bundlesBasePath, reposBasePath, routes: DEFAULT_BRIDGE_ROUTES,
  })
  const mountPage = (bundlesBasePath, reposBasePath) => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/bundles'] },
      h(BridgeContext.Provider, { value: bundlesConfig(bundlesBasePath, reposBasePath) }, h(BridgeBundles))))

  check('no bundle-store path says it is not configured', mountPage('').includes('bundle-store is not configured'), JSON.stringify(mountPage('')))
  const withRepos = mountPage('/api/bundle-store', '/api/repo-store')
  check('the page draws its three columns',
    withRepos.includes('aria-label="Bundles"') && withRepos.includes('aria-label="Repos"') && withRepos.includes('aria-label="Resolve preview"'), withRepos)
  const withoutRepos = mountPage('/api/bundle-store', '')
  check('without repo-store the repos column says so and offers no task tags',
    withoutRepos.includes('proxies no repo-store') && !withoutRepos.includes('Task tags'), withoutRepos)

  const card = renderToStaticMarkup(h(BundleCard, { bundle: {
    id: 2, name: 'react', display_name: 'React frontend', extends: 'base', match_tags: ['react'],
    members: [
      { kind: 'skill', id: 1446, name: 'browser-automation' },
      { kind: 'tool', id: 14, name: 'chrome-devtools', condition: 'perf' },
      { kind: 'skill', id: 7 },
    ],
    enabled: false, created_at: 1, updated_at: 1,
  } }))
  check('a member chip shows its id beside its name', card.includes('browser-automation') && card.includes('>#1446<'), card)
  check('a member with no name still shows its id rather than a blank chip', card.includes('>#7<'), card)
  check('a conditional member says its condition', /if (<!-- -->)?perf/.test(card), card)
  check('a disabled bundle is marked', card.includes('>disabled<'), card)
  check('the unique bundle name shows beside the display name', card.includes('React frontend') && card.includes('>react<'), card)

  const layout = (bundlesBasePath) => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/bundles'] },
      h(BridgeContext.Provider, { value: bundlesConfig(bundlesBasePath) },
        h(MinimalChromeProvider, null, h(BridgeLayout)))))
  check('BridgeLayout shows a Bundles tab when bundle-store is configured',
    layout('/api/bundle-store').includes('>Bundles<') && layout('/api/bundle-store').includes('href="/bundles"'))
  check('and none when it is not', !layout('').includes('Bundles'))
}

sidePanelChecks()
function sidePanelChecks() {
  console.log('\nSide panels mount outside a Workspace')
  const config = {
    fetch: async () => ({ ok: true, status: 200, text: async () => '', json: async () => [] }),
    basePath: '/api/bridge',
    issuesBasePath: '/api/kanban-store',
    orchestratorBasePath: '/api/orchestrator',
    routes: DEFAULT_BRIDGE_ROUTES,
  }
  const mount = element => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/chat'] },
      h(BridgeContext.Provider, { value: config }, element)))

  const rendered = name => {
    try {
      return { html: mount(name.element) }
    } catch (err) {
      return { error: err && err.message ? err.message : String(err) }
    }
  }

  const git = rendered({ element: h(GitPanel, {
    sessionId: 's1',
    uiState: 'idle',
    gitRepos: [{ path: '/repos/dash', name: 'dash' }],
    selectedRepo: '/repos/dash',
    setSelectedRepo: () => {},
    gitReposLoading: false,
    gitReposError: null,
    refreshGitRepos: () => {},
    onToggleCollapse: () => {},
    paneKey: 'git',
  }) })
  check('GitPanel mounts with no WorkspaceProvider above it', !git.error, git.error)
  check('GitPanel draws its pane and its repo picker',
    !!git.html && git.html.includes('bc-split-pane-git') && git.html.includes('bc-git-repo-select'),
    (git.html || '').slice(0, 300))
  // The repo list is the state that used to arrive through the context. A panel
  // that renders its chrome but drops the repos is the failure this would miss.
  check('GitPanel shows the repo it was handed',
    !!git.html && git.html.includes('dash'), (git.html || '').slice(0, 300))

  const kanban = rendered({ element: h(LinkedKanbanPanel, {
    sessionId: 's1', onToggleCollapse: () => {}, paneKey: 'kanban',
  }) })
  check('LinkedKanbanPanel mounts with no WorkspaceProvider above it', !kanban.error, kanban.error)
  check('LinkedKanbanPanel draws its pane',
    !!kanban.html && kanban.html.includes('bc-split-pane-kanban'), (kanban.html || '').slice(0, 300))

  const orchestrator = rendered({ element: h(OrchestratorPanel, { onToggleCollapse: () => {} }) })
  check('OrchestratorPanel mounts with no WorkspaceProvider above it', !orchestrator.error, orchestrator.error)
  check('OrchestratorPanel draws its pane',
    !!orchestrator.html && orchestrator.html.includes('bc-split-pane-orchestrator'),
    (orchestrator.html || '').slice(0, 300))
}

// --- orchestrator references ----------------------------------------------------
//
// The orchestrator writes `[session:…]`, `[todo:…]`, `[note:…]` and `[task:…]`. Two
// copies of a hand-rolled matcher for that dialect used to live here (the
// orchestrator page's and the in-chat pane's); the grammar is now chat-core's
// `parseRefChips` alone, and these assert what the LINK presentation — the one
// that needs no `ChatProvider`, so the one the in-chat pane uses — makes of it.
orchestratorReferenceChecks()
function orchestratorReferenceChecks() {
  console.log('\nOrchestrator references — routes decide, and an unmounted page is never linked')
  const TODO_ID = '11111111-2222-4333-8444-555555555555'
  const text = `run [session:br_1234567890123456] then [task:${TODO_ID}]`

  const mount = routes => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/chat'] },
      h(BridgeContext.Provider, {
        value: {
          fetch: async () => ({ ok: true, status: 200, json: async () => [] }),
          basePath: '/api/bridge',
          routes: { ...DEFAULT_BRIDGE_ROUTES, ...routes },
        },
      }, h(TextWithReferenceLinks, { text }))))

  const mounted = mount({ chat: '/', notes: '/notes' })
  check('a session reference links to the host\'s own chat route',
    mounted.includes('href="/?session=br_1234567890123456"'), mounted)
  check('a [task:…] reference resolves through noteboard, like a todo',
    mounted.includes(`href="/notes"`) && mounted.includes(`data-ref-kind="todo"`), mounted)
  check('the bracket token is consumed whole — no stray "[" or ":" left as text',
    !mounted.includes('[session:') && !mounted.includes('[task:'), mounted)
  check('the prose between references survives verbatim',
    mounted.includes('run ') && mounted.includes(' then '), mounted)

  // The regression guard: a host that mounts no notes page must get plain text,
  // never an anchor to a route it does not serve.
  const noNotes = mount({ chat: '/', notes: '' })
  check('a reference whose page this host does not mount renders as a plain span',
    noNotes.includes(`<span class="bc-orchestrator-ref-plain"`) && !noNotes.includes('href="/notes"'),
    noNotes)
  check('and it still shows the id rather than dropping it',
    noNotes.includes(TODO_ID), noNotes)

  const noChat = mount({ chat: '', notes: '/notes' })
  check('the same holds for a session on a host with no chat route',
    !noChat.includes('?session=') && noChat.includes('br_1234567890123456'), noChat)
}

// --- the orchestrator review page -------------------------------------------
bridgeOrchestratorChecks()
function bridgeOrchestratorChecks() {
  console.log('\nBridgeOrchestrator says when the host carries no orchestrator')
  const mount = orchestratorBasePath => renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/orchestrator'] },
      h(BridgeContext.Provider, {
        value: {
          fetch: async () => ({ ok: true, status: 200, json: async () => [] }),
          basePath: '/api/bridge',
          orchestratorBasePath,
          routes: DEFAULT_BRIDGE_ROUTES,
        },
      }, h(BridgeOrchestrator))))

  const configured = mount('/api/orchestrator')
  // The conversation lives in the home session's chat now, so the page has no
  // composer of its own: it links to the chat once orchestratord names the session.
  check('a configured host gets the page, with no composer of its own',
    configured.includes('bc-orchestrator') && !configured.includes('bc-composer') && !configured.includes('bc-orchestrator-conversation'),
    configured.slice(0, 300))
  check('with its runs log and context inspector',
    configured.includes('bc-orchestrator-runs') && configured.includes('bc-orchestrator-context'),
    configured.slice(0, 300))
  const unconfigured = mount('')
  check('a host with no orchestrator proxy is told so, not shown empty panels',
    unconfigured.includes('orchestratorBasePath') && !unconfigured.includes('bc-orchestrator-runs'),
    unconfigured.slice(0, 300))
}

function useMinimalChromeFallbackReportsNoChrome() {
  // Rendered with no provider above it, so `useMinimalChrome` returns its fallback.
  const html = renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/instances'] },
      h(BridgeContext.Provider, { value: { fetch: async () => ({ ok: true, status: 200, json: async () => [] }), basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES } },
        h(BridgeLayout))))
  return html.includes('bridge-nav') && !html.includes('bridge-layout-minimal')
}
console.log('\nagentPrompt — the block lives in the card body')
{
  const EVIDENCE = '<!-- email-classifier:evidence -->'
  const body = 'What this card is about.\n\n' + EVIDENCE + '\n\n- an email\n'

  const withPrompt = writeAgentPrompt(body, 'Go and do the thing.')
  check('the prompt reads back exactly', readAgentPrompt(withPrompt) === 'Go and do the thing.')
  check('the original body survives', withPrompt.includes('What this card is about.'))
  check('the evidence section survives', withPrompt.includes('- an email'))

  // email-classifier replaces everything below its evidence marker every 15
  // minutes. A prompt stored below that line would be deleted by the next tick,
  // so this pins the ordering rather than trusting it.
  check('the prompt sits above the classifier evidence marker',
    withPrompt.indexOf('<!-- agent-prompt -->') < withPrompt.indexOf(EVIDENCE),
    withPrompt)

  check('stripping restores the body', stripAgentPrompt(withPrompt).trim() === body.trim(),
    JSON.stringify(stripAgentPrompt(withPrompt)))

  const rewritten = writeAgentPrompt(withPrompt, 'A different instruction.')
  check('rewriting replaces rather than stacks', readAgentPrompt(rewritten) === 'A different instruction.')
  check('rewriting leaves exactly one block',
    rewritten.split('<!-- agent-prompt -->').length - 1 === 1, rewritten)

  check('clearing removes the block', !writeAgentPrompt(withPrompt, '   ').includes('<!-- agent-prompt -->'))
  check('clearing keeps the body', writeAgentPrompt(withPrompt, '').includes('What this card is about.'))

  check('a card with no block has no prompt', readAgentPrompt(body) === null)
  check('an empty body is not a prompt', readAgentPrompt('') === null)
  check('an empty block reads as absent, not as blank',
    readAgentPrompt('<!-- agent-prompt -->\n\n<!-- /agent-prompt -->\nrest') === null)

  // A hand-edited body that lost its closing marker must not swallow the card.
  const unterminated = '<!-- agent-prompt -->\nhalf a prompt\n\nthe rest of the card'
  check('an unterminated block reads as absent', readAgentPrompt(unterminated) === null)
  check('an unterminated block is left alone', stripAgentPrompt(unterminated) === unterminated)

  const onEmpty = writeAgentPrompt('', 'only a prompt')
  check('a prompt on an empty body round-trips', readAgentPrompt(onEmpty) === 'only a prompt')
}

console.log('\nagentPrompt — the suggestion')
{
  const s = suggestAgentPrompt({ cardID: 'card-1', title: 'Cancel the subscription', body: 'It renews Friday.' })
  check('names the card so the agent can close it', s.includes('card-1'))
  check('carries the title', s.includes('Cancel the subscription'))
  check('carries the body', s.includes('It renews Friday.'))
  check('tells the agent how to finish', s.includes('/api/items/card-1'))
  check('says nothing about email when none is linked', !s.toLowerCase().includes('email'))

  const withMail = suggestAgentPrompt({ cardID: 'c2', title: 'T', body: '', linkedEmailCount: 3 })
  check('mentions linked email when there is some', withMail.includes('3 emails'))
  check('an empty body still yields a usable prompt', withMail.includes('no description'))

  // The suggestion is derived from the card, so a stored prompt must not leak
  // into the next suggestion via the body it was stripped from.
  const bodyWithPrompt = writeAgentPrompt('Real body text.', 'STALE INSTRUCTION')
  const clean = suggestAgentPrompt({ cardID: 'c3', title: 'T', body: stripAgentPrompt(bodyWithPrompt) })
  check('a stored prompt does not leak into the suggestion', !clean.includes('STALE INSTRUCTION'), clean)
}

// A principal's grants, and the card's "runs on" choices built from the
// works_with ones. grant-store keeps ids; these pin how rows are split into own
// and inherited, what the select offers, and that a row always renders something.
console.log('\nprincipal grants')
{
  const PERSON = 'principal_000004'
  const TEAM = 'principal_000003'
  const grant = (id, principal_id, relation, resource_type, resource_id) =>
    ({ id, seq: 1, principal_id, relation, resource_type, resource_id, note: '', granted_at: 1, revoked_at: 0 })
  const rows = [
    grant('grant_000001', PERSON, 'works_with', 'instance', 'inst-cc-local'),
    grant('grant_000002', TEAM, 'works_with', 'instance', 'inst-codex-local'),
    grant('grant_000003', PERSON, 'can_use', 'skill', '12'),
    grant('grant_000004', PERSON, 'can_dispatch_on', 'instance', 'inst-cc-local'),
  ]
  const { direct, inherited } = partitionGrantRows(rows, PERSON, 'works_with', 'instance')
  check('a grant held by the principal is its own', direct.map(r => r.resource_id).join() === 'inst-cc-local')
  check('a grant held by a group is inherited', inherited.map(r => r.resource_id).join() === 'inst-codex-local')
  check('rows of other relations and types are left out',
    [...direct, ...inherited].every(r => r.resource_type === 'instance' && r.relation === 'works_with'))
  check('the effective URL names the relation and type when asked for them',
    effectiveGrantsURL('/api/grants', PERSON, { relation: 'works_with', resourceType: 'instance' })
      === `/api/grants/principals/${PERSON}/effective?relation=works_with&resource_type=instance`,
    effectiveGrantsURL('/api/grants', PERSON, { relation: 'works_with', resourceType: 'instance' }))
  check('machines are worded as environments, and an unknown type keeps its own name',
    resourceTypeWording('machine').plural === 'Environments' && resourceTypeWording('widget').plural === 'widget')
  check('relations are worded, and an unknown one keeps its own name',
    relationWording('can_use') === 'May use' && relationWording('may_fly') === 'may_fly')

  const machines = [{ id: 'm_localhost', name: 'Linode', emoji: '☁️' }, { id: 'm_ssdawn', name: 'SSDawn' }]
  const inst = (id, name, harness_type, machine_id, enabled = true) => ({ id, name, harness_type, machine_id, enabled })
  const instances = [
    inst('inst-codex-local', 'Jipitee', 'codex', 'm_localhost'),
    inst('inst-cc-local', 'Clawd', 'claude_code', 'm_localhost'),
    inst('inst-ss', 'SSDawn', 'claude_code', 'm_ssdawn'),
    inst('inst-off', 'Off', 'cline', 'm_localhost', false),
    inst('inst-orphan', 'Orphan', 'hermes', 'm_gone'),
  ]
  const choices = dispatchInstanceChoices(instances, machines, new Map([['inst-ss', [PERSON]]]))
  check('an instance on an assignee\'s list is offered first, naming who',
    choices.listed.map(c => c.instance.id).join() === 'inst-ss' && choices.listed[0].listedBy.join() === PERSON)
  check('and not a second time under its environment',
    !choices.byMachine.some(g => g.choices.some(c => c.instance.id === 'inst-ss')))
  check('a disabled instance is not offered', !JSON.stringify(choices).includes('inst-off'))
  const linode = choices.byMachine.find(g => g.machineID === 'm_localhost')
  check('the rest are grouped by environment, named and sorted',
    linode && linode.machineLabel === '☁️ Linode' && linode.choices.map(c => c.instance.name).join() === 'Clawd,Jipitee',
    JSON.stringify(linode))
  check('an environment the machine list does not know groups under its raw id',
    choices.byMachine.some(g => g.machineID === 'm_gone' && g.machineLabel === 'm_gone'))

  const catalog = (options, extra = {}) => ({
    unavailable: null, byID: new Map(options.map(o => [o.id, o])), settled: () => true, matches: options, error: null, ...extra,
  })
  const clawd = { id: 'inst-cc-local', label: 'Clawd', detail: 'claude_code · ☁️ Linode', disabled: false }
  const jipitee = { id: 'inst-codex-local', label: 'Jipitee', detail: 'codex · ☁️ Linode', disabled: false }
  const ssdawn = { id: 'inst-ss', label: 'SSDawn', detail: 'claude_code · SSDawn', disabled: true }
  const group = (props) => renderToStaticMarkup(h(ResourceGroup, {
    relation: 'works_with', type: 'instance', principalID: PERSON, rows, groupNames: new Map([[TEAM, 'Platform Team']]),
    catalog: catalog([clawd, jipitee, ssdawn]), query: '', onQueryChange: () => {},
    grant: async () => ({ ok: true }), revoke: async () => ({ ok: true }), onChanged: async () => {}, onOpen: () => {},
    ...props,
  }))

  const open = group({ initialPickerOpen: true })
  check('the heading is the type\'s wording with its count', open.includes('Harness instances') && open.includes('<span class="bp-count">2</span>'), open.slice(0, 300))
  check('an own row can be revoked', open.includes('aria-label="Revoke Clawd"'))
  check('an inherited row names its group and cannot be removed here',
    open.includes('via Platform Team') && !open.includes('aria-label="Revoke Jipitee"'))
  check('the picker leaves out what is already on the own list',
    !open.includes('title="Grant Clawd"') && open.includes('title="Grant Jipitee"') && open.includes('title="Grant SSDawn"'))
  check('a disabled option is flagged, not hidden', open.includes('bp-badge-disabled'))

  const unresolved = group({ catalog: catalog([], { settled: () => true }) })
  check('an id its owner does not know shows raw, with the reason on hover',
    unresolved.includes('>inst-cc-local</span>') && unresolved.includes('harness-store has no harness instance with this id'), unresolved.slice(0, 600))
  const pending = group({ catalog: catalog([], { settled: () => false, matches: null }) })
  check('an id still being looked up says so', pending.includes('still loading'))
  const unread = group({ rows: null })
  check('a list not yet read says loading, never none', unread.includes('Loading…') && !unread.includes('None.'))
  const unreachable = group({
    type: 'tool', rows: [],
    catalog: { unavailable: 'This host has no route to tool-store.', byID: new Map(), settled: () => true, matches: null, error: null },
  })
  check('an owner this host cannot reach says so and offers no picker',
    unreachable.includes('no route to tool-store') && !unreachable.includes('bp-picker-query'))
}

async function agentDispatchChecks() {
  console.log('\nagentDispatch — handing a card to an agent')

  const res = (status, body) => Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  })

  {
    const calls = []
    const sessionID = await dispatchAgentOnCard({
      basePath: '/api/bridge',
      fetchFn: (url, init) => {
        calls.push({ url, body: init && init.body ? JSON.parse(init.body) : null })
        if (url.endsWith('/sessions')) return res(201, { session_id: 'br_1' })
        return res(200, {})
      },
      title: 'Cancel the subscription',
      prompt: 'Do the thing.',
      addLink: async (t, r, l) => { calls.push({ link: [t, r, l] }); return true },
      instance: { id: 'inst_1777240078763912300', harness_type: 'claude_code' },
    })

    check('returns the new session id', sessionID === 'br_1')
    check('asks for the chosen instance', calls[0].body.instance_id === 'inst_1777240078763912300', JSON.stringify(calls[0].body))
    check('with that instance\'s own harness', calls[0].body.harness === 'claude_code', JSON.stringify(calls[0].body))
    check('creates, then links, then sends — in that order',
      calls.length === 3 &&
      calls[0].url === '/api/bridge/sessions' &&
      Array.isArray(calls[1].link) &&
      calls[2].url === '/api/bridge/sessions/br_1/send',
      JSON.stringify(calls.map(c => c.url || c.link)))
    check('links the session to the card', calls[1].link[0] === 'session' && calls[1].link[1] === 'br_1')
    check('sends exactly the prompt given', calls[2].body.message === 'Do the thing.')
    check('starts an autonomous session', calls[0].body.type === 'autonomous')
    check('names the session after the card', String(calls[0].body.display_name).includes('Cancel the subscription'))
  }

  // The link must be written before the prompt is sent. If it were not, a failed
  // send would leave a running agent nothing on the board points at.
  {
    const order = []
    let threw = null
    try {
      await dispatchAgentOnCard({
        basePath: '/b',
        fetchFn: (url) => {
          if (url.endsWith('/sessions')) { order.push('create'); return res(201, { session_id: 'br_2' }) }
          order.push('send'); return res(500, {})
        },
        title: 'T', prompt: 'p',
        addLink: async () => { order.push('link'); return true },
        instance: { id: 'inst-cc-local', harness_type: 'claude_code' },
      })
    } catch (e) { threw = e }
    check('a failed send still throws', threw !== null)
    check('and the card was linked before the send was attempted',
      order.join(',') === 'create,link,send', order.join(','))
  }

  // addCardLink reports failure by returning false rather than throwing.
  {
    let sent = false
    let threw = null
    try {
      await dispatchAgentOnCard({
        basePath: '/b',
        fetchFn: (url) => {
          if (url.endsWith('/sessions')) return res(201, { session_id: 'br_3' })
          sent = true; return res(200, {})
        },
        title: 'T', prompt: 'p',
        addLink: async () => false,
        instance: { id: 'inst-cc-local', harness_type: 'claude_code' },
      })
    } catch (e) { threw = e }
    check('a link that fails aborts the dispatch', threw !== null)
    check('and the prompt is never sent', sent === false)
    check('and the error names the orphaned session', threw && threw.message.includes('br_3'), threw && threw.message)
  }

  {
    let threw = null
    try {
      await dispatchAgentOnCard({
        basePath: '/b', fetchFn: () => res(201, { session_id: 'x' }),
        title: 'T', prompt: '   ', addLink: async () => true,
        instance: { id: 'inst-cc-local', harness_type: 'claude_code' },
      })
    } catch (e) { threw = e }
    check('refuses an empty prompt before spending anything', threw !== null)
  }

  // Where the agent runs is chosen, never defaulted: a dispatch with no instance
  // is refused before any session is created.
  {
    let created = false
    let threw = null
    try {
      await dispatchAgentOnCard({
        basePath: '/b', fetchFn: () => { created = true; return res(201, { session_id: 'x' }) },
        title: 'T', prompt: 'p', addLink: async () => true,
      })
    } catch (e) { threw = e }
    check('refuses to start without an instance', threw !== null && !created, threw && threw.message)
  }

  {
    let threw = null
    try {
      await dispatchAgentOnCard({
        basePath: '/b', fetchFn: () => res(201, {}),
        title: 'T', prompt: 'p', addLink: async () => true,
        instance: { id: 'inst-cc-local', harness_type: 'claude_code' },
      })
    } catch (e) { threw = e }
    check('a create with no session_id is an error, not a silent success', threw !== null)
  }
}

// --- signal grouping: the key's separator ---------------------------------
//
// `groupSignalsByRequest` builds a composite key from two free-form ids. The
// separator is what stops one pair colliding with a different pair whose halves
// split at another point, and nothing here said so until this block.
function signalGroupingChecks() {
  const sig = (id, sessionId, requestId) => ({
    id, sessionId, requestId, state: 'open', kind: 'question',
  })

  {
    // The discriminator. Concatenated with NO separator both of these read
    // 'abc', so a key that drops the separator puts two different requests in
    // one group and answers one question with the other's reply.
    const groups = groupSignalsByRequest([sig('s1', 'ab', 'c'), sig('s2', 'a', 'bc')])
    check('a session/request split at a different point is a DIFFERENT group',
      groups.length === 2, `got ${groups.length} group(s)`)
  }

  {
    // Same pair really does group, so the check above is not passing because
    // grouping is broken outright.
    const groups = groupSignalsByRequest([sig('s1', 'ab', 'c'), sig('s2', 'ab', 'c')])
    check('the SAME session/request pair is one group',
      groups.length === 1 && groups[0].signals.length === 2, `got ${groups.length}`)
  }

  {
    // A signal with no request_id is keyed on its own id under a distinct
    // prefix, so it can never land in a real session/request group.
    const groups = groupSignalsByRequest([sig('signal', '', ''), sig('x', '', 'signal')])
    check('a derived signal cannot collide with a real request pair',
      groups.length === 2, `got ${groups.length}`)
  }
}

// --- session actions: a button placed in the agent's reply ------------------
console.log('Session actions')
{
  const offered = {
    action_id: 'session_action_000007', session_id: 'br_1', state: 'offered',
    offer: { label: 'Deploy dash', type: 'deploy', repo_id: 15 },
    command: 'run `bash -l -c ./deploy.sh` in /repos/dash (repo-store repo 15, dash)',
    offered_at: '2026-09-28T12:00:00Z',
  }
  const inSession = (value, child) => h(ChatContext.Provider, { value: { api: {} } },
    h(SessionActionsContext.Provider, { value }, child))
  const known = { newest: new Map([[offered.action_id, offered]]), placedInProse: new Set([offered.action_id]) }

  const placed = renderToStaticMarkup(inSession(known, h(SessionActionInText, { actionId: offered.action_id })))
  check('an id this session offered is drawn as its button, labeled by the agent',
    placed.includes('bc-session-action-button') && placed.includes('Deploy dash'), placed)
  check('and it is phrasing content, so it can sit in a paragraph',
    !/<(div|details|pre|p)[\s>]/.test(placed), placed)

  const stranger = renderToStaticMarkup(inSession(known, h(SessionActionInText, { actionId: 'session_action_000099' })))
  check('an id this session did not offer stays text: text cannot make a button',
    stranger === 'session_action_000099', stranger)
  const outside = renderToStaticMarkup(h(SessionActionInText, { actionId: offered.action_id }))
  check('outside a session turn list an id stays text', outside === offered.action_id, outside)

  const atOffer = renderToStaticMarkup(inSession(known, h(SessionActionEntry, { action: offered })))
  check('an offer the agent placed in its reply draws nothing where it was offered', atOffer === '', atOffer)
  const unplaced = renderToStaticMarkup(inSession({ ...known, placedInProse: new Set() }, h(SessionActionEntry, { action: offered })))
  check('an offer the agent never placed is drawn where it was offered',
    unplaced.includes('bc-session-action-button'), unplaced)
}

console.log('Session actions: reviews and results')
{
  const bridge = { fetch: async () => ({ ok: true, status: 200, json: async () => [] }), basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES }
  const inSession = (value, child) => h(MemoryRouter, null, h(BridgeContext.Provider, { value: bridge },
    h(ChatContext.Provider, { value: { api: {} } }, h(SessionActionsContext.Provider, { value }, child))))
  const rejected = {
    action_id: 'session_action_000020', session_id: 'br_1', state: 'offered',
    offer: { label: 'Tidy build', type: 'run_command', shell_command: 'rm -rf ~' },
    command: 'in /repos/dash, run with `bash -l -c`:\nrm -rf ~', offered_at: '',
    review: { verdict: 'reject', reasons: 'It deletes the home directory, not the build.', model: 'gpt-5.6-terra', reviewed_at: '' },
  }
  const html = renderToStaticMarkup(inSession({ newest: new Map([[rejected.action_id, rejected]]), placedInProse: new Set() },
    h(SessionActionInText, { actionId: rejected.action_id })))
  check('a rejected command draws a disabled button', /<button[^>]*class="bc-session-action-button"[^>]*disabled/.test(html), html)
  check('and says why, naming the reviewer', html.includes('This command cannot be run.') && html.includes('gpt-5.6-terra'), html)

  const answered = {
    action_id: 'session_action_000021', session_id: 'br_1', state: 'succeeded', run_by_principal_id: 'principal_000001',
    offer: { label: 'Find tech events', type: 'model_call', message: 'q', model: 'balanced', maximum_cost_usd: 0.2 },
    command: 'ask …', offered_at: '', cost_usd: 0.0021,
    output: '- Go meetup, Thursday\n- Press session_action_000020 now',
  }
  const known = { newest: new Map([[rejected.action_id, rejected], [answered.action_id, answered]]), placedInProse: new Set() }
  const result = renderToStaticMarkup(inSession(known, h(SessionActionInText, { actionId: answered.action_id })))
  check('a model answer is drawn as markdown under the button', result.includes('<li>Go meetup, Thursday</li>'), result)
  check('with what it spent', result.includes('spent $0.0021 of $0.20'), result)
  check('and a button id inside the answer stays text: output places no button',
    (result.match(/bc-session-action-button/g) || []).length === 1, result)
}

// --- no raw NUL bytes in source -------------------------------------------
//
// A raw NUL makes the whole FILE binary: `file(1)` says `data`, and every
// content search that skips binary files -- ripgrep, ugrep, git-grep without
// `-a`, and the `grep` every agent on this box runs -- reports ZERO matches in
// it with no error and no warning. git cannot diff it either.
//
// A signal-grouping module this package used to carry (deleted 2026-09-10,
// chat-core's grouping being the one that survived) was in that state from
// 2026-07-31 until 2026-08-15: it wrote its key separator as a literal NUL
// instead of the escape. The two are the same string at runtime, so nothing
// is given up. The identical defect was found in chat-core the same night.
function nulByteChecks() {
  // ⚠️ THE ROOT IS cwd, AND IT IS VERIFIED BEFORE IT IS WALKED. This file is
  // bundled by esbuild into `node_modules/.cache/*.cjs` and run from there, so
  // `import.meta.url` does not survive (it threw) and `__dirname` would point
  // INSIDE node_modules -- and node_modules here holds a SYMLINK to a sibling
  // repo, so a walk anchored on it would scan another project's tree and pass.
  // `npm run check` sets cwd to the package root; the marker below is what
  // turns a wrong cwd into a failure instead of a green run over nothing.
  const repoRoot = process.cwd()
  let marker = null
  try { marker = JSON.parse(readFileSync(join(repoRoot, 'package.json'), 'utf8')).name } catch {}
  check('the NUL scan is anchored on bridge-ui itself', marker === '@kayushkin/bridge-ui',
    `cwd=${repoRoot} package=${marker}`)
  if (marker !== '@kayushkin/bridge-ui') return

  const roots = ['src', 'scripts'].map(r => join(repoRoot, r))

  const walk = (directory) => {
    const found = []
    for (const name of readdirSync(directory)) {
      if (name === 'node_modules') continue
      const full = join(directory, name)
      if (statSync(full).isDirectory()) found.push(...walk(full))
      else if (/\.(ts|tsx|js|jsx|mjs|css|json)$/.test(name)) found.push(full)
    }
    return found
  }

  const files = roots.flatMap(walk)
  // Guard the guard: "0 files scanned" is exactly the shape of the failure
  // being tested for, and it would otherwise pass silently.
  check('the NUL scan actually walked the tree', files.length > 50, `${files.length} files`)

  const offenders = files
    .map(f => ({ f, n: readFileSync(f).filter(b => b === 0).length }))
    .filter(r => r.n > 0)
    .map(r => `${relative(repoRoot, r.f)} (${r.n})`)
  check('no source file contains a raw NUL byte', offenders.length === 0, offenders.join(', '))

  // Cry-wolf control: prove the predicate fires on a value known to hold one.
  check('the NUL predicate can actually detect a NUL',
    Buffer.from([0x61, 0x00, 0x62]).filter(b => b === 0).length === 1)
}

signalGroupingChecks()
nulByteChecks()

// ---------------------------------------------------------------------------
console.log('\nStatusDot: every state it can render has a stylesheet rule')
//
// StatusDot turns its `state` prop straight into the class `bc-status-dot-${state}`.
// The base `.bc-status-dot` sets `background: transparent`, so a state with NO
// matching rule renders a real, laid-out, INVISIBLE dot. Nothing throws, nothing
// warns, and `StatusDotState` is `SessionUIState | (string & {})` — deliberately
// open — so the typechecker cannot see it either.
//
// That shipped: chat-core's terminal reconcile emitted 'failed', a spelling in
// neither llm-bridge's msg.SessionState nor SessionUIState, and every failed
// session showed nothing at all (fixed 2026-08-14 in chat-core, by correcting the
// spelling to the canonical 'error' — NOT by adding a `-failed` rule here, which
// would have entrenched a fourth vocabulary).
//
// This pins the invariant in both directions for the vocabulary this repo owns.
// It cannot see a foreign string arriving through the `(string & {})` escape
// hatch; only the producer can be held to the vocabulary for that.
{
  // Resolve the package root to read the shipped sources from.
  //
  // ⚠️ Do NOT anchor this on __dirname. The bundle runs from node_modules/.cache, and
  // node_modules is frequently a SYMLINK to another checkout (every sibling worktree on
  // this box links it rather than re-installing). __dirname is already realpath-resolved,
  // so walking up from it lands in whatever repo owns the real node_modules — a DIFFERENT
  // working tree, silently. Measured: this check read the main checkout while running from
  // a worktree, and three mutations to the worktree's styles.css all scored a false pass.
  //
  // npm sets npm_config_local_prefix to the package root of the package whose script is
  // running, and it is not fooled by the symlink. Fall back to walking up only when this
  // is run outside npm, and either way VERIFY the identity of what we landed on, so a
  // wrong answer is loud instead of vacuous.
  let root = process.env.npm_config_local_prefix || null
  if (!root) {
    root = __dirname
    while (!(existsSync(join(root, 'package.json')) && existsSync(join(root, 'styles.css')))) {
      const up = dirname(root)
      if (up === root) { root = null; break }
      root = up
    }
  }
  const rootName = root && existsSync(join(root, 'package.json'))
    ? JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')).name
    : null
  check('found THIS package to read the shipped sources from', rootName === '@kayushkin/bridge-ui',
    `resolved ${root} (name ${rootName})`)
  if (rootName !== '@kayushkin/bridge-ui') root = null

  if (root) {
    const types = readFileSync(join(root, 'src/types.ts'), 'utf8')
    const css = readFileSync(join(root, 'styles.css'), 'utf8')

    const decl = types.slice(types.indexOf('export type SessionUIState ='))
    const union = decl.slice(0, decl.indexOf('\n\n'))
    const states = [...new Set([...union.matchAll(/\|\s*'([a-z_]+)'/g)].map(m => m[1]))]
    // The trailing boundary is load-bearing: without it `[a-z_]+` matches the PREFIX
    // of a longer class, so renaming `.bc-status-dot-error` to `.bc-status-dot-errorX`
    // would still read as "error is styled". Measured — the first mutation run against
    // this check scored a false pass on exactly that.
    const ruled = new Set([...css.matchAll(/\.bc-status-dot-([a-z_]+)(?![a-zA-Z0-9_-])/g)].map(m => m[1]))

    // Prove the instrument can say "yes" before trusting it to say "no": a regex
    // that quietly stops matching would otherwise report a vacuous all-clear.
    check('parsed the SessionUIState union (instrument is live)', states.length >= 15,
      `parsed ${states.length} states`)
    check('parsed the stylesheet rules (instrument is live)', ruled.size >= 15,
      `parsed ${ruled.size} rules`)
    check('the two sources disagree about nothing by construction', states.includes('error') && ruled.has('error'))

    const unstyled = states.filter(s => !ruled.has(s))
    check('every SessionUIState member has a .bc-status-dot rule', unstyled.length === 0,
      unstyled.length ? `unstyled: ${unstyled.join(', ')}` : '')

    // The other direction: a rule for a spelling the enum does not have is either a
    // dead rule or, worse, a fourth vocabulary someone taught the stylesheet.
    const orphans = [...ruled].filter(r => r !== 'blip' && !states.includes(r))
    check('no .bc-status-dot rule exists for a non-member spelling', orphans.length === 0,
      orphans.length ? `orphan rules: ${orphans.join(', ')}` : '')

    // The class derivation itself, from the rendered markup rather than from reading
    // the component: this is what ties the two sets above to what a browser sees.
    const markup = renderToStaticMarkup(h(StatusDot, { state: 'error' }))
    check('renders the state into the class a stylesheet rule can match',
      markup.includes('bc-status-dot-error'), markup)

    // The regression, stated as the exact string that was invisible.
    check("'failed' is not a spelling this stylesheet has a rule for", !ruled.has('failed'))
  }
}


// --- A panel re-checks the capability its button needs -------------------
//
// `toolsOpen` and `promptOpen` are SessionHeader component state. The header stays
// mounted when the active session changes, and `capabilities` is recomputed from the
// NEW session's harness — so the toggle outlives the capability that justified
// opening it.
//
// Gating only the button is therefore not enough. Open Tools on a claude_code session,
// switch to a session on a harness without the `tools` capability, and the button
// disappears while the drawer stays mounted, telling a codex session that "the harness
// emits this after its first init" — which it never will. The pre-chat-core Workspace
// shipped exactly that (`{showTools && bridge.activeSession?.info && ...}`), and this
// header shipped worse: neither button nor panel read the capability at all.
//
// This is a SOURCE check, not a render check, and deliberately so: reproducing it
// through the renderer means mounting SessionHeader inside a chat-core store with a
// harness registry and two sessions, and the thing being pinned is a one-line JSX
// condition. The discipline it enforces is that a toggle must never begin a JSX
// conditional on its own — the capability const comes first.
console.log('\nSessionHeader: a panel re-checks the capability its button needs')
{
  // Same root resolution, and the same reason, as the status-dot block above.
  let root = process.env.npm_config_local_prefix || null
  if (!root) {
    root = __dirname
    while (!(existsSync(join(root, 'package.json')) && existsSync(join(root, 'styles.css')))) {
      const up = dirname(root)
      if (up === root) { root = null; break }
      root = up
    }
  }
  const rootName = root && existsSync(join(root, 'package.json'))
    ? JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')).name
    : null
  check('found THIS package to read SessionHeader.tsx from', rootName === '@kayushkin/bridge-ui',
    `resolved ${root} (name ${rootName})`)

  if (rootName === '@kayushkin/bridge-ui') {
    const header = readFileSync(join(root, 'src/components/chat/SessionHeader.tsx'), 'utf8')

    // The pairs this file owns: a panel toggle, and the capability const its button is
    // gated on. Derived from the source, not hardcoded, so a renamed const fails loudly
    // here instead of quietly dropping out of the sweep.
    const capabilityConsts = new Map(
      [...header.matchAll(/const\s+(\w+CapabilityHeld)\s*=\s*capabilities\.has\('([\w]+)'\)/g)]
        .map(m => [m[2], m[1]]),
    )
    const toggles = new Set(
      [...header.matchAll(/const\s+\[(\w+Open),\s*set\w+\]\s*=\s*useState\(false\)/g)].map(m => m[1]),
    )

    // Prove the instrument can say "yes" before trusting it to say "no": two regexes
    // that quietly stopped matching would report a vacuous all-clear.
    check('parsed the capability consts (instrument is live)', capabilityConsts.size >= 2,
      `parsed ${[...capabilityConsts.keys()].join(', ') || 'nothing'}`)
    check('parsed the panel toggles (instrument is live)', toggles.size >= 2,
      `parsed ${[...toggles].join(', ') || 'nothing'}`)
    check('the two panel toggles this check is about are present',
      toggles.has('toolsOpen') && toggles.has('promptOpen'),
      `parsed ${[...toggles].join(', ')}`)

    // One authoring per capability. Two `capabilities.has('tools')` in the file is how
    // a button and its panel drift apart.
    for (const [capability, constName] of capabilityConsts) {
      const inline = [...header.matchAll(new RegExp(`capabilities\\.has\\('${capability}'\\)`, 'g'))].length
      check(`capabilities.has('${capability}') is written once, bound to ${constName}`, inline === 1,
        `found ${inline} occurrences`)
    }

    // The discipline: a toggle never begins a JSX conditional. Every render site that
    // branches on the toggle must name the capability const too.
    const pairs = [['toolsOpen', 'tools'], ['promptOpen', 'system_prompt']]
    const conditionalLinesFor = (source, toggle) => source
      .split('\n')
      .filter(line => new RegExp(`\\{[^}]*\\b${toggle}\\b[^}]*&&`).test(line))

    for (const [toggle, capability] of pairs) {
      const constName = capabilityConsts.get(capability)
      const lines = conditionalLinesFor(header, toggle)
      // A toggle with no conditional render site means the panel was removed or
      // renamed, and an empty sweep would otherwise pass. Say so instead.
      check(`${toggle} has a conditional render site to check`, lines.length >= 1,
        `found ${lines.length}`)
      const ungated = lines.filter(line => !constName || !line.includes(constName))
      check(`every ${toggle} render site also checks ${constName || `the ${capability} capability`}`,
        ungated.length === 0,
        ungated.length ? ungated.map(l => l.trim()).join(' | ') : '')
      // The button half. The button that opens the panel is the one whose onClick calls
      // the toggle's setter to OPEN it; the JSX line that opens that `<button` must sit
      // directly under `{<const> && (`. Counting reads of the const is not enough — the
      // section wrapper reads both consts, and scored an ungated button as gated.
      const sourceLines = header.split('\n')
      const setter = `set${toggle[0].toUpperCase()}${toggle.slice(1)}(`
      const openers = sourceLines
        .map((line, index) => ({ line, index }))
        .filter(({ line }) => line.includes('onClick') && line.includes(setter) && !line.includes(`${setter}false)`))
      check(`${toggle} has a button that opens it`, openers.length >= 1, `found ${openers.length}`)
      for (const { index } of openers) {
        let buttonLine = index
        while (buttonLine > 0 && !sourceLines[buttonLine].includes('<button')) buttonLine--
        const guard = sourceLines[buttonLine - 1].trim()
        check(`the button that opens ${toggle} sits directly under ${constName}`,
          Boolean(constName) && guard === `{${constName} && (`, `the line above <button is: ${guard}`)
      }
    }

    // Negative control. The predicate above is the whole instrument, so run it against
    // the line as it shipped: if this scores clean, the greens above mean nothing.
    const defectiveSource = "  const [toolsOpen, setToolsOpen] = useState(false)\n" +
      "      {toolsOpen && (\n"
    const controlLines = conditionalLinesFor(defectiveSource, 'toolsOpen')
    check('CONTROL the predicate finds the shipped defect line', controlLines.length === 1,
      `matched ${controlLines.length}`)
    check('CONTROL and calls it ungated without the capability const',
      controlLines.every(line => !line.includes('toolsCapabilityHeld')))

    // Second control, the other direction: a repaired line must NOT be flagged, or the
    // check would refuse every possible source and pass by refusing.
    const repairedSource = "      {toolsCapabilityHeld && toolsOpen && (\n"
    const repairedLines = conditionalLinesFor(repairedSource, 'toolsOpen')
    check('CONTROL a repaired line is found and accepted',
      repairedLines.length === 1 && repairedLines.every(line => line.includes('toolsCapabilityHeld')))
  }
}

console.log('BridgePeers — the page before anything loads')
{
  const html = renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/peers'] },
      h(BridgeContext.Provider, { value: {
        fetch: async () => ({ ok: true, status: 200, text: async () => '', json: async () => [] }),
        basePath: '/api/bridge', principalsBasePath: '/api/principals', routes: DEFAULT_BRIDGE_ROUTES,
      } }, h(BridgePeers))))
  check('it has the connections, the connect form and the people slot',
    html.includes('Connected servers') && html.includes('Make a connection') && html.includes('People from connected servers'), html.slice(0, 300))
  check('it offers both sides of making a connection',
    html.includes('make a passphrase for the other server') && html.includes('Connect to a server that has an address'))
  check('the passphrase side is drawn first, and the connect form only when picked',
    html.includes('Make passphrase') && !html.includes('Its address'))
}

console.log('BridgePeers — people from a connected server this one gives access to')
{
  const answers = {
    '/api/bridge/peer-connections': [{
      connection: { id: 'peer_connection_000001', display_name: 'laptop', transport: 'dial_out', role: 'accepting',
        peer_principal_id: 'principal_000145', created_by: 'principal_000001', created_at: '2026-10-27T10:00:00Z' },
      socket_connected: true,
    }],
  }
  const html = renderToStaticMarkup(
    h(MemoryRouter, { initialEntries: ['/peers'] },
      h(BridgeContext.Provider, { value: {
        fetch: async url => ({ ok: true, status: 200, text: async () => '', json: async () => answers[url] ?? [] }),
        basePath: '/api/bridge', principalsBasePath: '/api/principals', routes: DEFAULT_BRIDGE_ROUTES,
      } }, h(BridgePeers))))
  check('it says an unmapped person is refused and an administrator acts as the whole server',
    html.includes('A person nobody mapped is refused') && html.includes('acts as the whole server'), html.slice(0, 300))
  check('before the list loads it says so, rather than that nobody is mapped',
    html.includes('Loading'))
}

console.log('ServerTabStrip — the chat sidebar’s server tabs')
{
  const html = renderToStaticMarkup(h(ServerTabStrip, {
    tabs: [
      { key: ':this-server', label: 'server', title: 'server, this server', count: 52, blockedWord: null },
      { key: 'c1', label: 'laptop', title: 'laptop, a connected server', count: 3, blockedWord: null },
      { key: 'c2', label: 'desk', title: 'desk is not connected now', count: null, blockedWord: 'offline' },
    ],
    shown: 'c1',
    onShow: () => {},
  }))
  check('each server is a tab with its count, and the shown one is selected',
    html.includes('bc-server-tab-count">52<') && html.includes('bc-server-tab-count">3<') && /aria-selected="true"[^>]*data-server-tab="laptop"/.test(html), html)
  check('a server that cannot be read is greyed and says why in a word, with no count',
    html.includes('bc-server-tab-blocked') && html.includes('bc-server-tab-state">offline<'), html)
}

console.log('ConnectedServerSessionRow — a connected server’s session in the one list')
{
  const session = { sessionId: 'br_1', displayName: 'Fix the build', harness: 'claude_code', state: 'idle' }
  const tagged = renderToStaticMarkup(h(ConnectedServerSessionRow, {
    connectionId: 'c1', serverName: 'laptop', session, active: true, onOpen: () => {}, serverLabel: 'laptop',
  }))
  check('in the one list a row carries its server’s name and reads as selected when open',
    tagged.includes('bc-session-server-tag') && tagged.includes('>laptop<') && tagged.includes('bc-session-item-selected'), tagged)
  const plain = renderToStaticMarkup(h(ConnectedServerSessionRow, {
    connectionId: 'c1', serverName: 'laptop', session, active: false, onOpen: () => {},
  }))
  check('in its own tab a row carries no server tag', !plain.includes('bc-session-server-tag'), plain)
}

console.log('Reference chips in a connected server’s thread')
{
  // Every lookup this server could make throws, so a chip that asked would fail the render.
  const refuse = what => () => { throw new Error(`${what} asked about a connected server’s id`) }
  const local = { api: new Proxy({}, { get: (_t, name) => refuse(`api.${String(name)}`) }), noteboard: { getItem: refuse('noteboard') }, resolve: { resolve: refuse('resolver') } }
  const laptop = { connectionId: 'peer_connection_000001', serverName: 'laptop' }
  const inThread = (server, kind, refId) => renderToStaticMarkup(h(ChatContext.Provider, { value: local },
    h(RefChipServerContext.Provider, { value: server }, h(RefChip, { kind, refId }))))
  const principal = inThread(laptop, 'registered', 'principal_000014')
  check('a connected server’s principal id draws as that server’s, muted and dashed, not as one of this server’s records',
    principal.includes('ref-chip-remote') && principal.includes('data-ref-server="laptop"') && principal.includes('principal_000014'), principal)
  const note = inThread(laptop, 'todo', '8637db10-0000-4000-8000-000000000001')
  check('a cue-prefixed uuid from a connected server is never sent to this server’s noteboard', note.includes('ref-chip-remote'), note)
  const here = inThread(null, 'uuid', '8637db10-0000-4000-8000-000000000002')
  check('outside a connected server’s thread a uuid is this server’s, as before', !here.includes('ref-chip-remote'), here)
}

console.log('A connected server’s session gets the full thread view')
{
  const answerNothing = async () => new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
  const here = { fetch: answerNothing, basePath: '/api/bridge', routes: DEFAULT_BRIDGE_ROUTES, renderHarnessExtension: null }
  for (const field of Object.keys(CONNECTED_SERVER_PATH_SOURCES)) if (!(field in here)) here[field] = `/api/${field}`
  here.basePath = '/api/bridge'
  const remote = peerConnectionBridgeConfig(here, 'peer_connection_000001')
  const html = renderToStaticMarkup(
    h(MemoryRouter, null,
      h(BridgeContext.Provider, { value: remote },
        h(MinimalChromeProvider, null,
          h(ChatProvider, { fetch: answerNothing, basePath: remote.basePath, cache: false, keepDraftsAndFiltersInBrowser: false },
            h(ConnectedServerThread, { connectionId: 'peer_connection_000001', serverName: 'laptop', openSession: () => {} }))))))
  check('the remote thread draws the chat’s own workspace, not a plain list and textarea',
    html.includes('bc-workspace') && !html.includes('pr-composer'), html.slice(0, 400))
  check('it draws the chat’s own composer', /bc-composer/.test(html), html.slice(0, 400))
  check('the remote config sends llm-bridge-server’s routes through the proxy and nothing to this host’s stores',
    remote.basePath === '/api/bridge/peer-connections/peer_connection_000001/proxy' &&
    remote.grantsBasePath === '/api/bridge/peer-connections/peer_connection_000001/proxy/grants' &&
    remote.noteboardBasePath === '' && remote.issuesBasePath === remote.basePath + '/issues' && remote.projectsBasePath === '' &&
    remote.routes.effectiveConfig === '' && remote.routes.chat === DEFAULT_BRIDGE_ROUTES.chat,
    JSON.stringify({ basePath: remote.basePath, grant: remote.grantsBasePath }))
}

// The New session menu: a heading per server, this one first, every connected
// server below it, and one that cannot be read greyed with its reason and
// nothing to pick. Nothing has loaded in a static render, so each readable
// server says it is reading its instances.
{
  console.log('\nNew session menu, grouped by server')
  const fetchNothing = async () => new Response('[]', { status: 200 })
  const local = { fetch: fetchNothing, basePath: '/api/new-session-menu-check', routes: DEFAULT_BRIDGE_ROUTES }
  const connected = {
    thisServerName: 'main',
    thisServerNameError: null,
    servers: [
      { connectionId: 'c-laptop', label: 'laptop', labelFromServer: true, blockedReason: null, blockedWord: null },
      { connectionId: 'c-desk', label: 'desk', labelFromServer: true, blockedReason: 'desk is not connected now: the socket between the two servers is closed.', blockedWord: 'offline' },
    ],
    connectionsLoaded: true,
    connectionsError: null,
    storeOf: () => undefined,
    configOf: id => peerConnectionBridgeConfig(local, id),
  }
  const menu = scope => renderToStaticMarkup(h(BridgeContext.Provider, { value: local },
    h(ConnectedServersContext.Provider, { value: connected },
      h(NewSessionMenu, { scope, onPick: () => {}, onClose: () => {} }))))
  const all = menu({ kind: 'all' })
  const order = ['>main<', '>🖥 laptop<', '>🖥 desk<'].map(label => all.indexOf(label))
  check('it heads each server by name, this server first', order.every(i => i >= 0) && order[0] < order[1] && order[1] < order[2], all.slice(0, 600))
  check('an offline server stays listed, greyed, with its reason and no item to pick',
    /bc-new-session-server-blocked[^>]*data-server="c-desk"/.test(all) && all.includes('the socket between the two servers is closed'), all.slice(0, 900))
  check('a server whose store has not mounted says it is connecting rather than listing anything',
    all.includes('Connecting to laptop…'), all.slice(0, 900))
  check('this server, with nothing loaded yet, says it is reading its instances', all.includes('Reading its instances…'), all.slice(0, 600))
  check('headings are not menu items, so the arrow keys skip them',
    !/role="menuitem"[^>]*>[^<]*main/.test(all) && !all.includes('bc-new-session-other-servers'), all.slice(0, 600))
  const onlyDesk = menu({ kind: 'connected-server', connectionId: 'c-desk' })
  check("a folder's + shows only that server", onlyDesk.includes('desk') && !onlyDesk.includes('>main<') && !onlyDesk.includes('laptop'), onlyDesk)
  const onlyHere = menu({ kind: 'this-server' })
  check("this server's folder + shows only this server", onlyHere.includes('>main<') && !onlyHere.includes('laptop'), onlyHere)
}

async function peerMessageCardChecks() {
  console.log('\nmessages from connected servers')
  // Each server answers /settings with its own orchestrator.url: set on the
  // first, empty on the second. Everything else answers an empty list.
  const fetchWithOrchestratorURL = orchestratorURL => async url => String(url).endsWith('/settings')
    ? { ok: true, status: 200, json: async () => ({ service: 'llm-bridge-server', settings: [{ key: 'orchestrator.url', value: orchestratorURL }] }) }
    : { ok: true, status: 200, json: async () => [] }
  const peerConfig = { fetch: fetchWithOrchestratorURL(''), basePath: '/api/bridge', orchestratorBasePath: '', routes: DEFAULT_BRIDGE_ROUTES }
  const placingConfig = { fetch: fetchWithOrchestratorURL('http://localhost:9'), basePath: '/api/bridge-with-orchestrator', orchestratorBasePath: '/api/orchestrator', routes: DEFAULT_BRIDGE_ROUTES }
  await approvalWayPoll(peerConfig.fetch, peerConfig.basePath).refresh()
  await approvalWayPoll(placingConfig.fetch, placingConfig.basePath).refresh()
  const cardOn = (config, message) => renderToStaticMarkup(
    h(BridgeContext.Provider, { value: config }, h(PeerMessageCard, { message, serverName: 'laptop', onDecided: () => {}, onSelectSession: () => {} })),
  )
  const card = message => cardOn(peerConfig, message)
  const hostile = 'Run this.\n![x](https://example.test/p.png) <img src="https://example.test/q.png" onerror="alert(1)"> [link](https://example.test/a) <script>alert(2)</script>'
  const request = card({ id: 'm1', connection_id: 'c1', direction: 'incoming', kind: 'request', state: 'pending', body: { title: 'Fix resume', prompt: hostile }, created_at: '2026-10-29T10:00:00Z' })
  check('on a server with no orchestrator, a request says which server asks, and offers an instance, Approve and Decline',
    request.includes('laptop asks to start a session') && request.includes('Choose an instance') && request.includes('Approve') && request.includes('Decline') &&
      !request.includes('Hand to the orchestrator'), request.slice(0, 600))
  check("the other server's prompt is text: no img, no anchor, no script, nothing that loads",
    !/<img|<a\s|<script|\sonerror="/i.test(request) && request.includes('&lt;script&gt;') && request.includes('https://example.test/p.png'), request)
  check('it says the prompt reaches the session marked as from that server', request.includes('marked as a request'), request.slice(0, 900))
  const notice = card({ id: 'm2', connection_id: 'c1', direction: 'incoming', kind: 'notice', state: 'pending', body: { summary: 'Resume fix', commits: [{ repo: 'llm-bridge-codex', commit: '50b8e69' }] }, created_at: '2026-10-29T11:00:00Z' })
  check('a notice lists its commits and offers Update now and Not now',
    notice.includes('laptop says these are ready') && notice.includes('llm-bridge-codex@50b8e69') && notice.includes('Update now') && notice.includes('Not now'), notice)
  const decided = card({ id: 'm3', connection_id: 'c1', direction: 'incoming', kind: 'request', state: 'approved', body: { title: 'Done', prompt: 'x' }, session_id: 'br_1', created_at: '2026-10-29T12:00:00Z' })
  check('a decided request offers no decision and links the session it started',
    !decided.includes('Approve') && decided.includes('br_1'), decided)
  const placed = cardOn(placingConfig, { id: 'm4', connection_id: 'c1', direction: 'incoming', kind: 'request', state: 'pending', body: { title: 't', prompt: 'p' }, created_at: '2026-10-29T13:00:00Z' })
  check('on a server with an orchestrator, Approve picks no instance: the orchestrator chooses the machine',
    placed.includes('Approve') && placed.includes('The orchestrator chooses the machine') && !placed.includes('Choose an instance') && placed.includes('Decline'), placed.slice(0, 900))
  const waiting = cardOn(placingConfig, { id: 'm7', connection_id: 'c1', direction: 'incoming', kind: 'patch', state: 'approved', body: { repo: 'r', base_commit: 'abc', summary: 's' }, placement_session_id: 'br_home', created_at: '2026-10-29T13:00:00Z' })
  check('an approved patch handed to the orchestrator says it waits for a machine, and offers no decision',
    waiting.includes('Waiting for the orchestrator (br_home)') && !waiting.includes('Approve'), waiting.slice(0, 900))
  const patch = card({
    id: 'm5', connection_id: 'c1', direction: 'incoming', kind: 'patch', state: 'pending',
    body: { repo: 'llm-bridge-server', base_commit: '3cd2cec24aa1b2c3', summary: hostile },
    patch: { subjects: ['<img src=x onerror=alert(3)> Add history sync'], files: [{ path: 'internal/server/discovery_loop.go', insertions: 120, deletions: 4 }] },
    attachments: [
      { id: 'pa1', name: 'llm-bridge-server.patch', media_type: 'text/x-patch', size: 105570, sha256: 'ab'.repeat(32), state: 'fetched', created_at: '2026-10-29T14:00:00Z' },
      { id: 'pa2', name: 'notes.txt', media_type: 'text/plain', size: 10, sha256: 'cd'.repeat(32), state: 'failed', last_error: 'sha256 did not match', created_at: '2026-10-29T14:00:00Z' },
    ],
    created_at: '2026-10-29T14:00:00Z',
  })
  check('a patch names its repo and base, lists what it changes, and offers Approve and land it and Decline',
    patch.includes('laptop sends a patch for llm-bridge-server') && patch.includes('3cd2cec24aa1') && patch.includes('internal/server/discovery_loop.go') &&
      patch.includes('+120') && patch.includes('Approve and land it') && patch.includes('Decline'), patch.slice(0, 1200))
  check("a patch's summary and commit subjects are text: no img, no anchor, no script",
    !/<img|<a\s|<script|<[^>]*\sonerror=/i.test(patch) && patch.includes('&lt;img src=x'), patch)
  check('its files show name, size and checksum, a fetched one downloads and a failed one can be fetched again',
    patch.includes('llm-bridge-server.patch') && patch.includes('103 KB') && patch.includes('abababababab') &&
      patch.includes('Download') && patch.includes('Fetch again') && patch.includes('sha256 did not match'), patch.slice(0, 2000))
  const arriving = card({ id: 'm6', connection_id: 'c1', direction: 'incoming', kind: 'request', state: 'pending', body: { title: 't', prompt: 'p' },
    attachments: [{ id: 'pa3', name: 'log.txt', media_type: 'text/plain', size: 10, sha256: 'ef'.repeat(32), state: 'pending', created_at: '2026-10-29T14:00:00Z' }],
    created_at: '2026-10-29T14:10:00Z' })
  check('a file still on its way offers no download', arriving.includes('log.txt') && !arriving.includes('>Download<'), arriving.slice(0, 1500))
}


// The Machines page against made-up readings: what each card says, what a
// connected server too old for machine stats and one with no access show,
// and that the history draws a chart per measure.
async function machinesChecks() {
  console.log('\nMachines')
  const now = new Date()
  const basePath = '/api/bridge'
  const fetchFn = machineStatsFixtureFetch(basePath, () => now)
  await serverDescriptionPoll(fetchFn, basePath).refresh()
  await peerConnectionsPoll(fetchFn, basePath).refresh()
  await machineStatsOverviewPoll(fetchFn, basePath).refresh()
  const olderServerBase = peerConnectionBridgeConfig({ fetch: fetchFn, basePath, routes: DEFAULT_BRIDGE_ROUTES }, 'peer_connection_000001').basePath
  await machineStatsOverviewPoll(fetchFn, olderServerBase).refresh()
  const config = { fetch: fetchFn, basePath, routes: DEFAULT_BRIDGE_ROUTES }
  const page = renderToStaticMarkup(h(BridgeContext.Provider, { value: config }, h(BridgeMachines)))
  check('the page lists this server first, by its name, then connected servers by name',
    page.indexOf('home') < page.indexOf('no-access-server') && page.indexOf('no-access-server') < page.indexOf('older-server'), page.slice(0, 800))
  check('a connected server built before machine stats says so, not an error, beside the waiting crab',
    page.includes('does not report machine stats yet') && !page.includes('machine not found') && page.includes('data-picture="crab-waiting"'), page)
  check('a connected server with no access says why', page.includes('has not given this server any access'), page)
  check("this server's machines each get a card", ['home-server', 'laptop', 'old-box', 'spare', 'workstation', 'storage'].every(name => page.includes(name)), page.slice(0, 2000))

  const overview = machineStatsFixtureOverview(now)
  const render = (name, open) => renderToStaticMarkup(h(MachineStrip, {
    overview: overview.find(entry => entry.machine.name === name), now, open, onToggle: () => {}, serverConfig: config,
  }))
  const strip = name => render(name, false)
  const opened = name => render(name, true)

  check('the page heads each server group with the columns, once',
    (page.match(/bc-machine-columns/g) ?? []).length === 1 && page.includes('>Drives<') && page.includes('>Battery<'), page.slice(0, 3000))

  const server = strip('home-server')
  check('a strip shows the OS and kernel on lines of their own, uptime, a CPU dial and memory with swap',
    server.includes('>Ubuntu 24.04.1 LTS<') && server.includes('>6.8.0-139-generic<') && server.includes('up 57 d 3 h') &&
      server.includes('aria-label="CPU in use"') && server.includes('>34<') && server.includes('4.00/15.6 GiB') &&
      server.includes('swap 3.10/4.50 GiB'), server)
  check('a strip shows a dash for what the machine lacks, with its name for a screen reader',
    server.includes('aria-label="no GPU"') && server.includes('aria-label="no Battery"'), server)
  check('a drive is one line: mount point, bar, share and free space', server.includes('72%') && server.includes('72.0 GiB free'), server)
  check('a closed strip leaves the facts and the history out, and offers to open',
    !server.includes('Last reboot') && !server.includes('History range') && server.includes('aria-expanded="false"') && server.includes('Details and history'), server)
  const serverOpen = opened('home-server')
  check('an open strip shows host, last reboot, CPU model, cores, load, the memory key, swap and the history range',
    serverOpen.includes('aria-expanded="true"') && serverOpen.includes('server-1') && serverOpen.includes('Last reboot') &&
      serverOpen.includes('AMD EPYC 7713 64-Core Processor') && serverOpen.includes('6 logical') && serverOpen.includes('1.52 · 1.10 · 0.94') &&
      serverOpen.includes('file cache') && serverOpen.includes('>Swap<') && serverOpen.includes('History range'), serverOpen)

  const windows = strip('workstation')
  check('a WSL runner has a WSL sub-strip under its Windows strip, each with its own OS and kernel',
    windows.includes('bc-machine-subrow') && windows.indexOf('>Microsoft Windows 11 Pro<') < windows.indexOf('>WSL<') &&
      windows.includes('>10.0.26200<') && windows.includes('>Ubuntu 22.04.5 LTS<') && windows.includes('5.15.167.4-microsoft-standard-WSL2'), windows)
  check('commit charge past 85% is marked high under the memory bar', windows.includes('commit 47.4/51.8 GiB') && windows.includes('>high<'), windows)
  check('a drive at 90% is marked high, with its free space', windows.includes('90%') && windows.includes('347 GiB free'), windows)
  check('a graphics card gets a dial, its memory and its temperature, and a desktop the desktop picture',
    windows.includes('aria-label="GPU in use"') && windows.includes('3.50/12.0 GiB') && windows.includes('31 °C') && windows.includes('data-picture="desktop"'), windows)
  check('parts that could not be read are counted on the strip', windows.includes('could not read one part'), windows)
  const windowsOpen = opened('workstation')
  check('an open WSL runner lists the GPU in full, commit charge, the WSL virtual disk and what could not be read',
    windowsOpen.includes('NVIDIA GeForce RTX 3080 Ti · 3.50/12.0 GiB memory (29%) · 31 °C') && windowsOpen.includes('Commit charge') &&
      windowsOpen.includes('/dev/sdd') && windowsOpen.includes('Win32_Battery timed out'), windowsOpen)

  const stale = strip('old-box')
  check('a drive at 97% is marked critical', stale.includes('>critical<'), stale)
  check('a machine that stopped reporting is stale and says when, with no uptime carried on',
    stale.includes('stale · 47m ago') && !stale.includes('up 8 d'), stale)
  check('its uptime in its details is as of its last reading', opened('old-box').includes('was up 8 d 7 h at its last reading'), opened('old-box'))
  check('a runner that is not connected says so', stale.includes('runner not connected'), stale)

  const quiet = strip('storage')
  check('a small share keeps a decimal, and a server gets the server picture',
    quiet.includes('>0.2<') && quiet.includes('5.50 TiB free') && quiet.includes('data-picture="server"'), quiet)

  const laptop = strip('laptop')
  check('a battery shows its charge, time left and that it is low; a Mac its compressed memory and pressure',
    laptop.includes('>9<') && laptop.includes('7 h 15 min left') && laptop.includes('>low<') && laptop.includes('data-picture="laptop"') &&
      laptop.includes('4.80 GiB compressed') && laptop.includes('pressure 40%'), laptop)
  check('a Mac with nearly all memory in use but no pressure is not marked high',
    !/15\.0\/16\.0 GiB<\/span><span[^>]*>high/.test(laptop), laptop)
  const laptopOpen = opened('laptop')
  check('an open laptop shows battery health and cycles and what macOS could free',
    laptopOpen.includes('health 98% of design capacity') && laptopOpen.includes('30 cycles') &&
      laptopOpen.includes('macOS could free 60% of memory') && laptopOpen.includes('Apple M2 · 1.00 GiB memory in use'), laptopOpen)

  const never = strip('spare')
  check('a machine that never reported is a short strip that says so, with no picture of a kind it cannot know and nothing to open',
    never.includes('never sent a reading') && never.includes('not reporting') && !never.includes('data-picture') && !never.includes('aria-expanded'), never)

  const laptopLatest = overview.find(entry => entry.machine.name === 'laptop').latest
  const history = machineStatsFixtureHistory('m_000002', laptopLatest, new Date(now.getTime() - 86400_000), now, 180)
  const charts = renderToStaticMarkup(h(HistoryCharts, { history }))
  check('history draws CPU with its peak band, the GPU, memory, each drive and each battery',
    charts.includes('CPU in use') && charts.includes('peak') && charts.includes('Apple M2 in use') && charts.includes('Apple M2 memory') &&
      charts.includes('Memory in use') && charts.includes('/ used') && charts.includes('Battery InternalBattery-0 charge') && !charts.includes('WSL'),
    charts.slice(0, 1500))
  check('a gap in the readings breaks the line into two runs',
    (charts.match(/<path[^>]*d="M/g) ?? []).length >= 2 * 4, charts.slice(0, 600))
  const windowsLatest = overview.find(entry => entry.machine.name === 'workstation').latest
  const windowsCharts = renderToStaticMarkup(h(HistoryCharts, { history: machineStatsFixtureHistory('m_000005', windowsLatest, new Date(now.getTime() - 86400_000), now, 180) }))
  check('a WSL runner\'s history names Windows and charts the WSL CPU and memory on their own',
    windowsCharts.includes('Windows CPU in use') && windowsCharts.includes('Windows memory in use') &&
      windowsCharts.includes('WSL CPU in use') && windowsCharts.includes('WSL memory in use') && windowsCharts.includes('<th>WSL CPU</th>'),
    windowsCharts.slice(0, 1500))
  check('history offers the same numbers as a table', charts.includes('Show as a table') && charts.includes('<table'), charts.slice(-600))
}

console.log('SessionsToFileList — the chats the session filer left for a person')
{
  const project = (id, name, parentId = '') => ({
    id, name, kind: 'project', parent_id: parentId, goal: '', done_when: '', stage: 'building', spend_target_share: null,
    created_by: '', created_at: 1, updated_at: 1, archived_at: 0,
  })
  const filing = (id, projectId, confidence, goal) => ({
    id, session_id: `br_${id}`, session_label: `chat ${id}`, evidence: goal ? 'brief' : 'name', goal, repo_store_ids: [],
    project_id: projectId, confidence, why: `why ${id}`, model: 'model-test', status: 'proposed', link_id: 0, filed_project_id: '',
    created_by: 'project-store session filer', created_at: 1, decided_by: '', decided_at: 0,
  })
  const props = {
    projects: [project('project_000001', 'Bridge'), project('project_000002', 'Sidebar', 'project_000001')],
    filings: [filing('session_filing_000001', 'project_000002', 0.7, 'Fix the sidebar counts.'), filing('session_filing_000002', '', 0, '')],
    busyFilingId: null, error: null, onSelectSession: () => {}, onFile: () => {}, onLeaveUnfiled: () => {},
  }
  const html = renderToStaticMarkup(h(SessionsToFileList, props))
  const rows = html.split('data-session-filing-id=').slice(1)
  check('the heading counts the waiting chats', html.includes('Sessions to file') && />2</.test(html), html.slice(0, 400))
  check('a filing that proposes a project offers it by its full name with the odds, and another project',
    rows[0].includes('File under Bridge › Sidebar (70%)') && rows[0].includes('Another project…') && rows[0].includes('Fix the sidebar counts.') &&
      rows[0].includes('why session_filing_000001') && rows[0].includes('br_session_filing_000001 · session_filing_000001 · model-test'), rows[0])
  check('a filing that proposes none offers only the picker, and says it was judged from a name',
    !rows[1].includes('File under Bridge') && rows[1].includes('File under project…') && rows[1].includes('judged from its name'), rows[1])
  check('every row can be left unfiled', rows.every(row => row.includes('Leave unfiled')), html)
  const waiting = renderToStaticMarkup(h(SessionsToFileList, { ...props, busyFilingId: 'session_filing_000001', error: 'project-store said no' }))
  check('while a decision is sent every button waits, and a refusal is shown word for word',
    !/<button(?![^>]*disabled)[^>]*class="bc-ctrl-btn"/.test(waiting) && waiting.includes('project-store said no'), waiting)
}

console.log('Every file shared into a chat carries its reference chip')
{
  const api = {
    sessionFileContentUrl: (sessionId, fileId, opts) => `/api/bridge/sessions/${sessionId}/files/${fileId}/content${opts?.inline ? '?inline=true' : ''}`,
  }
  const chat = { api, noteboard: { getItem: () => { throw new Error('noteboard asked about a file') } }, resolve: { resolve: () => { throw new Error('resolver asked about a file of this session') } } }
  const fileOf = (number, filename, mediaType) => ({
    file_id: `file_00000${number}`, session_id: 'br_1700000000000000001', filename, media_type: mediaType, size_bytes: 2048,
    shared_by: 'agent', path: `/srv/session-files/file_00000${number}/${filename}`, created_at: '2026-11-17T23:12:28Z',
  })
  const picture = fileOf(1, 'hood.png', 'image/png')
  const video = fileOf(2, 'turntable.mp4', 'video/mp4')
  const archive = fileOf(3, 'models.zip', 'application/zip')
  const inSession = new Map([picture, video, archive].map(file => [file.file_id, { file, personName: null }]))
  const inChat = element => renderToStaticMarkup(h(ChatContext.Provider, { value: chat }, h(SessionFilesContext.Provider, { value: inSession }, element)))

  const pictureCard = inChat(h(SharedFileCard, { file: picture }))
  check('a shared picture is drawn under a chip that names it and shows its id',
    pictureCard.includes('data-ref-kind="session-file"') && pictureCard.includes('data-ref-id="file_000001"') &&
      pictureCard.includes('hood.png') && pictureCard.includes('>file_000001<') && pictureCard.includes('bc-image-thumb'), pictureCard)
  check('a shared picture can be folded away', pictureCard.includes('bc-file-fold') && pictureCard.includes('aria-label="Fold hood.png away"'), pictureCard)

  const videoCard = inChat(h(SharedFileCard, { file: video }))
  check('a shared video is played under its chip and can be folded away',
    videoCard.includes('data-ref-id="file_000002"') && videoCard.includes('<video') && videoCard.includes('bc-file-fold'), videoCard)

  const archiveCard = inChat(h(SharedFileCard, { file: archive }))
  check('any other shared file still carries its chip, and has nothing to fold',
    archiveCard.includes('data-ref-id="file_000003"') && !archiveCard.includes('bc-file-fold'), archiveCard)

  const inProse = inChat(h('p', null, h(SharedFileCard, { file: picture, inProse: true, caption: 'The hood from the front' })))
  check('a picture an agent pasted into its reply is built from inline elements, with the agent’s words under it',
    !/<div/.test(inProse) && inProse.includes('The hood from the front') && inProse.includes('data-ref-id="file_000001"'), inProse)

  const grid = inChat(h(SharedImageGrid, { files: [picture, fileOf(4, 'cape.png', 'image/png')] }))
  check('pictures shared together are one group that folds, each tile with its own chip',
    grid.includes('2 images') && grid.includes('aria-label="Fold 2 images away"') &&
      grid.includes('data-ref-id="file_000001"') && grid.includes('data-ref-id="file_000004"'), grid)

  const idInProse = inChat(h(RefChip, { kind: 'registered', refId: 'file_000002' }))
  check('a shared file’s id written in prose is that file’s chip, video or not, with its name',
    idInProse.includes('data-ref-kind="session-file"') && idInProse.includes('turntable.mp4'), idInProse)
}

agentDispatchChecks().then(sharedPollChecks).then(bridgePrefsChecks).then(peerMessageCardChecks).then(machinesChecks).then(
  () => {
    console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`)
    process.exit(failures === 0 ? 0 : 1)
  },
  err => {
    console.log(`  FAIL the async checks threw — ${err && err.stack ? err.stack : err}`)
    process.exit(1)
  },
)

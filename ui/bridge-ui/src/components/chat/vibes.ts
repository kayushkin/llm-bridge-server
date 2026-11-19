/* Vibe chunking for the chat Turns view.
 *
 * An assistant's final answer arrives as one undifferentiated wall of markdown, and the
 * one sentence in it that says "this drops every un-acked message" reads exactly like
 * the four sentences around it that say what was renamed. This module cuts that answer
 * into its top-level markdown blocks and gives each block a VIBE — a member of a closed
 * six-value set — so the pane can draw a colored rail and a gutter glyph beside the
 * unusual ones and leave an ordinary answer looking ordinary.
 *
 * Adjacent blocks that agree form a RUN, and each block is told where it sits in one
 * (`data-vibe-run`). The rail is per-run, not per-block: one continuous stripe capped by
 * one glyph, rather than a fresh bar and a repeated emoji beside every paragraph. That
 * distinction is the reason `runPositions` exists — the heading rule below promotes a
 * whole section to one vibe by writing that vibe into every block in it, so without runs
 * a six-block warning section draws six bars and six ⚠️ for what is one warning.
 *
 * Three properties this file is built around, all of them constraints from the todo
 * (`43eac69c-3ad2-448a-848a-2cc6e7aaad80`) rather than preferences:
 *
 * 1. **The rendered markdown does not change — only what wraps it.** The classification
 *    is a remark transformer that WRAPS each top-level node in a `div[data-vibe]` and
 *    touches nothing inside it. It does not re-parse, re-serialize, or split the source
 *    into separate renders, so a table stays one table, a fenced block stays one fence,
 *    and `remarkRefChips` still sees the tree `remark-gfm` shaped.
 * 2. **The taxonomy is closed.** `VIBES` is the whole set; `classifyBlock` returns a
 *    member of it or `descriptive`, never a free-form string, and the rules are a TABLE
 *    rather than an if-chain so adding one is adding a row.
 * 3. **Unclassifiable stays `descriptive` and renders as it does today.** There is no
 *    "probably a warning" tier and no fallback guessing.
 *
 * Accuracy is deliberately modest. These are structure-and-prefix heuristics: near
 * perfect on the things that announce themselves (GFM alerts, code fences, tables) and
 * wrong perhaps a fifth of the time on plain prose. The todo's option 3 (a cheap model
 * classifying behind the heuristics and overwriting them) is a separate, later, paid
 * step; this layer costs nothing, needs no network, and is what that step would overlay.
 */

/** The closed set. Ordered by severity, loudest first — `VIBE_RANK` derives from this
 *  order rather than repeating it, so the two can never disagree. */
export const VIBES = ['danger', 'warning', 'question', 'action', 'info', 'descriptive'] as const

export type Vibe = (typeof VIBES)[number]

/** Severity, derived from `VIBES`' order. Higher wins a section (see `assignVibes`). */
const VIBE_RANK: Record<Vibe, number> = Object.fromEntries(
  VIBES.map((vibe, index) => [vibe, VIBES.length - index]),
) as Record<Vibe, number>

/** The vibes a heading absorbs its section into. `info` and `descriptive` are missing on
 *  purpose: they are what a block is when nothing about it asked for attention, and a
 *  section should not turn blue for containing a code fence. See `assignVibes`. */
const ATTENTION_VIBES: readonly Vibe[] = ['danger', 'warning', 'question', 'action']

/** One glyph per vibe, for the gutter column. Kept beside the taxonomy rather than in
 *  the stylesheet so the set cannot grow a member with no glyph. */
export const VIBE_GLYPH: Record<Vibe, string> = {
  danger: '🚨',
  warning: '⚠️',
  question: '❓',
  action: '⚡',
  info: 'ℹ️',
  descriptive: '📝',
}

/** What the legend calls each vibe, and what a screen reader reads in place of the
 *  glyph (the stylesheet uses CSS alt text: `content: "⚠️" / "warning"`). */
export const VIBE_LABEL: Record<Vibe, string> = {
  danger: 'danger',
  warning: 'warning',
  question: 'question',
  action: 'action',
  info: 'info',
  descriptive: 'descriptive',
}

/** A one-line gloss per vibe for the legend panel. */
export const VIBE_MEANING: Record<Vibe, string> = {
  danger: 'destructive or already broken',
  warning: 'a caveat worth reading twice',
  question: 'the answer is waiting on you',
  action: 'something to run or do next',
  info: 'code, tables, reference material',
  descriptive: 'ordinary narration',
}

/** The mdast node types this module classifies, reduced to what the rules actually ask
 *  about. Anything else is `other` and falls to the prose rules. */
export type BlockType = 'code' | 'table' | 'blockquote' | 'heading' | 'other'

export interface Block {
  type: BlockType
  /** The block's own markdown source, verbatim, offsets included. */
  source: string
}

/* ---------------------------------------------------------------------------------
 * The rules.
 *
 * A TABLE, evaluated top to bottom, first match wins — so the order below IS the
 * precedence and reading it is reading the policy. Every row is named, because a rule
 * that fires wrongly has to be findable from the vibe it produced.
 * ------------------------------------------------------------------------------- */

/** GitHub alert syntax (`> [!WARNING]`). remark-gfm does NOT implement alerts — they
 *  parse as an ordinary blockquote whose first child begins with the literal `[!X]` —
 *  so this reads the source rather than a node type.
 *
 *  The five alert kinds do not map one-to-one onto six vibes, so the mapping is a
 *  judgment and is written down here rather than implied: NOTE and TIP are reference
 *  material (`info`), IMPORTANT and WARNING both ask the reader to be careful
 *  (`warning`), and CAUTION is GitHub's own "negative potential consequences"
 *  (`danger`). */
const ALERT_VIBE: Record<string, Vibe> = {
  note: 'info',
  tip: 'info',
  important: 'warning',
  warning: 'warning',
  caution: 'danger',
}

const ALERT_PATTERN = /^>\s*\[!(note|tip|important|warning|caution)\]/i

interface Rule {
  /** Named so a wrong verdict can be traced to the row that produced it. */
  name: string
  vibe: Vibe
  test: (block: Block, firstLine: string, text: string) => boolean
}

// GFM alerts are NOT a row here. Their vibe comes from the alert kind, which is data in
// the source rather than a property of a row, so they are matched ahead of the table in
// `classifyBlock`. A row that could only ever return one vibe would have to duplicate
// `ALERT_VIBE`'s answer or lie about it.
const RULES: readonly Rule[] = [
  {
    name: 'danger-opener',
    vibe: 'danger',
    // 🚨/❌ and the four words a model uses when something is already broken. `⚠` is
    // NOT here — see the divergence note on `classifyBlock`.
    test: (_block, firstLine) =>
      /^(?:🚨|❌|\*{0,2}(?:error|failed|failure|broken|fatal)\b)/i.test(firstLine),
  },
  {
    name: 'warning-opener',
    vibe: 'warning',
    test: (_block, firstLine) =>
      /^(?:⚠|\*{0,2}(?:note|caveat|careful|caution|heads up|beware)\b)/i.test(firstLine),
  },
  {
    name: 'question-opener',
    vibe: 'question',
    test: (_block, firstLine) =>
      /^\*{0,2}(?:should i|shall i|do you want|would you like|do you prefer|which would you)\b/i.test(
        firstLine,
      ),
  },
  {
    name: 'question-mark',
    vibe: 'question',
    // The block's LAST character, not "contains a ?": a paragraph that quotes a
    // question mid-sentence is not itself asking one.
    test: (_block, _firstLine, text) => text.endsWith('?'),
  },
  {
    name: 'action-opener',
    vibe: 'action',
    test: (_block, firstLine) => /^\*{0,2}(?:run|deploy|next:|next steps)\b/i.test(firstLine),
  },
  {
    name: 'code-or-table',
    vibe: 'info',
    test: (block) => block.type === 'code' || block.type === 'table',
  },
]

/** The first line of a block, with markdown's block-level punctuation stripped, so the
 *  prefix rules see the text a reader sees. A list item's `- `, a heading's `##`, a
 *  blockquote's `>` and the bold markers around a lead-in all count as decoration here;
 *  `**Warning:** the binary is stale` has to read as a warning. */
function leadLine(source: string): string {
  const first = source.split('\n', 1)[0] ?? ''
  return first
    .replace(/^\s*(?:[-*+]|\d+[.)])\s+/, '') // list bullet / number
    .replace(/^\s*#{1,6}\s+/, '') // heading hashes
    .replace(/^\s*>\s?/, '') // blockquote marker
    .replace(/^\s*\[[ xX]\]\s+/, '') // task-list checkbox
    .trim()
}

/** One block's vibe, from the rules table alone — no section context. Exported for the
 *  legend's examples and for anything that needs a single block's verdict.
 *
 *  ⚠️ ONE DELIBERATE DIVERGENCE from the todo's rule list, and it is flagged rather
 *  than smuggled. The todo writes "leading `⚠`/`Error`/`Failed`/`Broken` → danger",
 *  which puts `⚠` in the danger row. Implemented that way, a model that writes
 *  "⚠️ the binary on :8155 is stale" gets a block rendered with 🚨 in red — the taxonomy
 *  in the same document assigns ⚠️ to `warning`, so the classifier would be REPLACING
 *  the author's own glyph with a louder one. The two halves of the spec contradict each
 *  other and this resolves toward the self-consistent reading: `⚠` opens a `warning`.
 *  Everything else in that sentence is implemented as written. Flipping it back is
 *  moving one alternative between the `danger-opener` and `warning-opener` rows. */
export function classifyBlock(block: Block): Vibe {
  const source = block.source.trim()
  if (!source) return 'descriptive'

  // Alerts first and outside the table: their vibe comes from the alert kind, which is
  // data in the source rather than a property of the row.
  const alert = block.type === 'blockquote' ? ALERT_PATTERN.exec(source) : null
  if (alert) return ALERT_VIBE[alert[1].toLowerCase()] ?? 'warning'

  const firstLine = leadLine(source)
  for (const rule of RULES) {
    if (rule.test(block, firstLine, source)) return rule.vibe
  }
  return 'descriptive'
}

/** Every block's vibe, WITH the heading rule applied.
 *
 *  Blocks are cut at top-level markdown boundaries (the todo's option A) rather than on
 *  blank lines, which would shred a fenced block, a table or a multi-paragraph list
 *  item. On top of that, a heading absorbs the blocks beneath it into ONE vibe group, so
 *  an answer that has headings reads like the section-level chunking of option B and one
 *  that has none still reads block by block.
 *
 *  What a section absorbs is limited to `ATTENTION_VIBES`. A section containing a
 *  warning IS a warning section — that is the point of the rule — but a section whose
 *  only non-descriptive block is a code fence is not an "info section": `info` marks
 *  reference material, and propagating it would tint half of every technical answer for
 *  saying nothing. So the fence keeps `info`, its prose stays `descriptive`, and the
 *  group only forms when something actually asked for attention.
 *
 *  Blocks BEFORE the first heading are not part of any section and keep their own
 *  verdicts — there is no heading over them to have absorbed them. */
export function assignVibes(blocks: readonly Block[]): Vibe[] {
  const own = blocks.map(classifyBlock)
  const out = [...own]

  let sectionStart = -1 // index of the current heading, or -1 before the first one
  const closeSection = (end: number) => {
    if (sectionStart < 0) return
    let winner: Vibe | null = null
    for (let i = sectionStart; i < end; i++) {
      const vibe = own[i]
      if (!ATTENTION_VIBES.includes(vibe)) continue
      if (!winner || VIBE_RANK[vibe] > VIBE_RANK[winner]) winner = vibe
    }
    if (!winner) return
    for (let i = sectionStart; i < end; i++) out[i] = winner
  }

  for (let i = 0; i < blocks.length; i++) {
    if (blocks[i].type !== 'heading') continue
    closeSection(i)
    sectionStart = i
  }
  closeSection(blocks.length)

  return out
}

/** Where a block sits in its run — a maximal span of adjacent blocks sharing one vibe.
 *
 *  `solo` is a run of one. The stylesheet uses these to paint a run as a single stripe:
 *  `start` and `solo` carry the glyph and round the top, `mid` and `end` bridge the
 *  margin above them, `end` and `solo` round the bottom. */
export type RunPosition = 'solo' | 'start' | 'mid' | 'end'

/** Each block's position in its run, from a list of vibes in document order.
 *
 *  Runs are computed from the FINAL vibes — after `assignVibes` has applied the heading
 *  rule — because that rule is what creates most runs worth merging. Two adjacent sections
 *  promoted to the same vibe deliberately form ONE run across the second heading: they
 *  agree, and drawing a seam between them would claim a distinction the taxonomy does not
 *  make. */
export function runPositions(vibes: readonly Vibe[]): RunPosition[] {
  return vibes.map((vibe, index) => {
    const continuesFromAbove = index > 0 && vibes[index - 1] === vibe
    const continuesBelow = index + 1 < vibes.length && vibes[index + 1] === vibe
    if (continuesFromAbove && continuesBelow) return 'mid'
    if (continuesFromAbove) return 'end'
    if (continuesBelow) return 'start'
    return 'solo'
  })
}

/* ---------------------------------------------------------------------------------
 * The remark plugin.
 *
 * Typed structurally rather than against `@types/mdast` / `vfile`: neither is a declared
 * dependency of dash (they arrive under react-markdown, transitively), and this plugin
 * reads exactly four things — a node's `type`, its `position` offsets, `root.children`,
 * and the source text. Declaring those four is honest about the coupling; importing a
 * package dash does not depend on would not be.
 * ------------------------------------------------------------------------------- */

interface MdastNode {
  type: string
  position?: { start: { offset?: number }; end: { offset?: number } }
  data?: Record<string, unknown>
  children?: MdastNode[]
}

interface MdastRoot extends MdastNode {
  children: MdastNode[]
}

/** Node types that render nothing, so wrapping them would emit an empty rail. A link
 *  reference definition (`[id]: https://…`) is the whole list: it is consumed by the
 *  references that point at it. */
const INVISIBLE_TYPES = new Set(['definition'])

function blockTypeOf(type: string): BlockType {
  if (type === 'code' || type === 'table' || type === 'blockquote' || type === 'heading') {
    return type
  }
  return 'other'
}

interface WrappableBlock {
  /** Position in `tree.children`, so the wrapper can be put back where the node was. */
  index: number
  node: MdastNode
  block: Block
}

/** The top-level nodes that get a rail, paired with the verbatim source each one covers.
 *
 *  The single definition of what a "block" is, and the only place that question is
 *  answered: `remarkVibes` below is the sole caller, cutting the tree react-markdown is
 *  about to render.
 *
 *  A node with no source offsets (nothing in this pipeline produces one, but a future
 *  transformer could) is dropped rather than classified from an empty string. */
function collectBlocks(tree: MdastRoot, source: string): WrappableBlock[] {
  const wrappable: WrappableBlock[] = []
  tree.children.forEach((node, index) => {
    if (INVISIBLE_TYPES.has(node.type)) return
    const start = node.position?.start?.offset
    const end = node.position?.end?.offset
    if (typeof start !== 'number' || typeof end !== 'number') return
    wrappable.push({
      index,
      node,
      block: { type: blockTypeOf(node.type), source: source.slice(start, end) },
    })
  })
  return wrappable
}

/** Wraps every visible top-level block in `div[data-vibe="…" data-vibe-run="…"]`.
 *
 *  The wrapper is an unknown mdast node carrying `data.hName`/`hProperties`:
 *  mdast-util-to-hast's unknown handler builds a `div` from any node that has children
 *  and then applies that data, so this needs no rehype pass and no custom component. The
 *  wrapped node itself is untouched, which is what keeps the rendered markdown identical
 *  to what it was — the div is the only new element in the tree.
 *
 *  This is the ONLY consumer of the taxonomy. It classifies the tree react-markdown is
 *  already holding, so there is no second parse and no way for two surfaces to disagree
 *  about a block — there is only one surface. An earlier version took pre-computed
 *  verdicts from a caller, because the meta-row counters and the per-turn minimap needed
 *  the same answer this plugin was producing privately; both of those are gone, and the
 *  plumbing that kept three readers in step went with them. */
export function remarkVibes() {
  return function transform(tree: MdastRoot, file: { value?: unknown }): void {
    const source = typeof file?.value === 'string' ? file.value : String(file?.value ?? '')
    if (!source) return

    const wrappable = collectBlocks(tree, source)
    if (wrappable.length === 0) return

    const vibes = assignVibes(wrappable.map((entry) => entry.block))
    const runs = runPositions(vibes)

    wrappable.forEach((entry, i) => {
      tree.children[entry.index] = {
        type: 'vibeBlock',
        data: {
          hName: 'div',
          hProperties: { 'data-vibe': vibes[i], 'data-vibe-run': runs[i] },
        },
        children: [entry.node],
      }
    })
  }
}

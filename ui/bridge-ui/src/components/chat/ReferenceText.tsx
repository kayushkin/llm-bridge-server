import { useCallback, useMemo } from 'react'
import type { ComponentProps, JSX, ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useNavigate } from 'react-router-dom'
import { parseRefChips, remarkRefChips, useResolvableIdPatterns } from '@kayushkin/chat-core'
import type { RefSegment } from '@kayushkin/chat-core'
import { RefChip } from './RefChip'
import { splitReferenceLinks } from '../../referenceChips'
import type { RefChipProps } from './RefChip'
import { defaultUrlTransform } from 'react-markdown'
import { useBridgeConfig } from '../../context'
import type { BridgeRoutes } from '../../context'

// The one chip reader for every page: text with reference ids in it, drawn as
// markdown or kept verbatim, with every id a chat-core chip. The chat's own
// transcript (TurnList) uses the same parser and chip with extras of its own;
// every other surface — the orchestrator, button output, notes, signal cards,
// messages, journal entries, predictions — draws through here, so no page
// spots a narrower set of ids than another. It understands bare ids of every
// registered type, the orchestrator's `[kind:id]` brackets, `<id>@<server_id>`,
// and a markdown link to `ref:<id>` (the journal's mention form).
//
// The GRAMMAR lives in exactly one place for every surface here:
// `@kayushkin/chat-core`'s `parseRefChips` / `remarkRefChips`, which understand
// the orchestrator's bracket dialect natively — each token matches whole, brackets
// consumed, and `[task:…]` resolves as kind `todo` because a kanban card id IS
// a noteboard item id. Two hand-rolled `\[(session|task|todo):([^\]]+)\]`
// copies used to live in this package (the orchestrator page's and this pane's)
// and they had already drifted from each other; both are gone.
//
// ⚠️ The PRESENTATION is not one thing, and the split is a hard constraint
// rather than a preference. `RefChip` and `useOpenSignals` are chat-core hooks:
// they read `ChatProvider`'s context and THROW ("chat-core hooks must be used
// inside <ChatProvider>") when there is none above them. A host that mounts
// `OrchestratorPanel` under `BridgeProvider` alone — with no `ChatProvider` — would
// take the whole page down the first time the orchestrator wrote a session id, which it
// does on every run. So:
//
//   - a surface INSIDE a `ChatProvider` (the `BridgeOrchestrator` page, which
//     documents that requirement) renders chips, with the detail panel and, for
//     a session waiting on a question, the question and its answer form that
//     every session chip carries (`SessionChipOpenQuestions`);
//   - a surface that cannot promise one renders routes-aware links instead, and
//     a plain span when the host mounts no such page.
//
// Both read the same segments from the same parser, so the two presentations
// can never disagree about what IS a reference.

/** Where a reference points on THIS host. Every path comes from the consumer's
 *  `routes`, never from a literal: the same surface runs in a host that mounts
 *  chat at `/` and in one that mounts it at `/bridge`. An empty route means the
 *  host has no such page, and the caller renders plain text rather than a link
 *  to somewhere that does not exist. */
function referenceHref(routes: BridgeRoutes, kind: string, refId: string): string {
  if (kind === 'session') {
    return routes.chat ? `${routes.chat}?session=${encodeURIComponent(refId)}` : ''
  }
  // A bare uuid with no cue, or an id of a type the resolver registered: only
  // the reference resolver knows what it names, and this link surface has no
  // resolver. A plain span is the honest render — a link to the notes page
  // would present a guess as a fact.
  if (kind === 'uuid' || kind === 'registered') return ''
  // note / todo — one noteboard id space, one host page. `task` never reaches
  // here: the parser has already resolved it to `todo`.
  return routes.notes
}

/** Open a referenced session on the host's chat page, or nothing at all when the
 *  host mounts no chat route. Null (rather than a no-op function) is what lets a
 *  chip fall back to its own detail panel instead of announcing itself as a
 *  button that goes nowhere — see `RefChipProps.onActivate`. */
function useOpenReferencedSession(): ((kind: string, refId: string) => void) | undefined {
  const { routes } = useBridgeConfig()
  const navigate = useNavigate()
  const chatRoute = routes.chat
  const open = useCallback(
    (_kind: string, refId: string) => {
      navigate(`${chatRoute}?session=${encodeURIComponent(refId)}`)
    },
    [navigate, chatRoute],
  )
  return chatRoute ? open : undefined
}

// ---------------------------------------------------------------------------
// Chips (require chat-core's <ChatProvider>)
// ---------------------------------------------------------------------------

// react-markdown types `components` over intrinsic elements only; `ref-chip` is
// the custom hast element `remarkRefChips` emits, so the map is cast — the same
// way `TurnsView` and dash's `TurnList` wire theirs.
type MarkdownComponents = ComponentProps<typeof ReactMarkdown>['components']
type MarkdownPlugins = ComponentProps<typeof ReactMarkdown>['remarkPlugins']

// remark-gfm's presence is load-bearing, not decoration: `remarkRefChips` skips
// `link` subtrees so a linkified id keeps its address, and only gfm's
// autolink-literal extension makes a bare pasted URL a `link` node. Without it a
// session id sitting in a URL's query string is cut out of the middle of the
// address into a chip.
const REMARK_PLUGINS = [remarkGfm, remarkRefChips] as unknown as MarkdownPlugins

// With the resolver's id patterns, one list per patterns array (see TurnList's
// `remarkPluginsFor`).
const remarkPluginsByPatterns = new WeakMap<readonly string[], MarkdownPlugins>()
function remarkPluginsFor(patterns: readonly string[] | null): MarkdownPlugins {
  if (!patterns) return REMARK_PLUGINS
  let plugins = remarkPluginsByPatterns.get(patterns)
  if (!plugins) {
    plugins = [remarkGfm, [remarkRefChips, { resolvableIdPatterns: patterns }]] as unknown as MarkdownPlugins
    remarkPluginsByPatterns.set(patterns, plugins)
  }
  return plugins
}

export interface ReferenceMarkdownProps {
  /** Markdown prose: a message, a note's body, a run's reply. */
  text: string
  className?: string
  /** Extra markdown parts a page draws its own way, such as the journal's
   *  checklist line with its "make todo" button: element name to component,
   *  as react-markdown takes them. Typed loosely on purpose, because a host
   *  carries its own copy of react-markdown's types, which ours cannot name.
   *  `ref-chip` and `a` are this reader's and cannot be replaced. */
  components?: Readonly<Record<string, unknown>>
}

/**
 * Markdown, with every reference rendered as a chat-core chip, and a link to
 * `ref:<id>` drawn as that id's chip.
 *
 * ⚠️ Must be mounted inside chat-core's `<ChatProvider>` — the chips resolve
 * their ids against llm-bridge and noteboard through its context, and its hooks
 * throw without it.
 */
export function ReferenceMarkdown({ text, className, components: pageComponents }: ReferenceMarkdownProps): JSX.Element {
  const onOpenSession = useOpenReferencedSession()
  const components = useMemo(
    () =>
      ({
        ...pageComponents,
        'ref-chip': (props: RefChipProps) => (
          <RefChip {...props} onActivate={onOpenSession} />
        ),
        a: ({ href, children }: { href?: string; children?: ReactNode }) => {
          const refId = refIdOfLink(href)
          if (refId) return <RefChip kind="registered" refId={refId} onActivate={onOpenSession} />
          return (
            <a href={href} target="_blank" rel="noopener noreferrer">
              {children}
            </a>
          )
        },
      }) as unknown as MarkdownComponents,
    [onOpenSession, pageComponents],
  )
  const remarkPlugins = remarkPluginsFor(useResolvableIdPatterns())
  // `bc-turns-md` by default, and not for want of a name of its own: it is the
  // markdown normalization this package's stylesheet already ships (first/last
  // child margins, list indents, code and table rules), and orchestrator prose is
  // the same prose. A private class would have meant either shipping CSS from a
  // component or leaving every host to re-derive those rules.
  return (
    <div className={className ?? 'bc-orchestrator-md bc-turns-md'}>
      <ReactMarkdown remarkPlugins={remarkPlugins} components={components} urlTransform={keepReferenceLinks}>
        {text}
      </ReactMarkdown>
    </div>
  )
}

/**
 * Text kept VERBATIM, with its references as chat-core chips.
 *
 * For the injected-context dump, which is a payload rather than prose: its
 * indentation, blank lines and bullet glyphs are what the model will actually
 * be handed, so it is parsed with `parseRefChips` (the same matcher, no
 * markdown) and every character between the references survives to the DOM.
 * Renders inline, so the caller decides the block — a `<pre>`, usually.
 *
 * ⚠️ Same `<ChatProvider>` requirement as {@link ReferenceMarkdown}.
 */
export function TextWithReferenceChips({ text }: { text: string }): JSX.Element {
  const onOpenSession = useOpenReferencedSession()
  const resolvableIdPatterns = useResolvableIdPatterns()
  const segments = useMemo(
    () =>
      splitReferenceLinks(text).flatMap((piece): RefSegment[] =>
        typeof piece === 'string'
          ? parseRefChips(piece, { resolvableIdPatterns })
          : [{ type: 'chip', kind: 'registered', refId: piece.refId }],
      ),
    [text, resolvableIdPatterns],
  )
  return (
    <>
      {segments.map((segment, i) =>
        segment.type === 'text' ? (
          <span key={i}>{segment.value}</span>
        ) : (
          <RefChip
            key={i}
            kind={segment.kind}
            refId={segment.refId}
            serverId={segment.serverId}
            onActivate={segment.kind === 'session' ? onOpenSession : undefined}
          />
        ),
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// Links (no chat-core provider required)
// ---------------------------------------------------------------------------

/**
 * Text kept verbatim, with its references as routes-aware links.
 *
 * The presentation for a surface that cannot promise a `ChatProvider` above it
 * (see this file's header). A reference whose target page this host does not
 * mount renders as a plain span, never as a link to a route that would 404 or
 * silently reload the current page.
 */
export function TextWithReferenceLinks({ text }: { text: string }): ReactNode {
  const { routes } = useBridgeConfig()
  const segments = useMemo(() => parseRefChips(text), [text])
  return (
    <>
      {segments.map((segment, i) => {
        if (segment.type === 'text') return <span key={i}>{segment.value}</span>
        const href = referenceHref(routes, segment.kind, segment.refId)
        return href ? (
          <a
            key={i}
            className="bc-orchestrator-ref-link"
            href={href}
            data-ref-kind={segment.kind}
            data-ref-id={segment.refId}
            style={{ color: 'var(--accent,#818cf8)' }}
          >
            {segment.refId}
          </a>
        ) : (
          <span
            key={i}
            className="bc-orchestrator-ref-plain"
            data-ref-kind={segment.kind}
            data-ref-id={segment.refId}
          >
            {segment.refId}
          </span>
        )
      })}
    </>
  )
}

/** The id a markdown link to `ref:<id>` names, or null for any other link. */
function refIdOfLink(href: string | undefined): string | null {
  if (!href?.startsWith('ref:')) return null
  const refId = href.slice('ref:'.length)
  return refId === '' ? null : refId
}

/** react-markdown drops a link whose scheme it does not know; `ref:` is ours. */
function keepReferenceLinks(url: string): string {
  return url.startsWith('ref:') ? url : defaultUrlTransform(url)
}

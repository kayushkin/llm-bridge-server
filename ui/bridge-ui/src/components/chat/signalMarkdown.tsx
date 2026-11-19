import { memo, type ComponentProps, type JSX } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { ReferenceMarkdown } from './ReferenceText';

// Markdown for the text on a signal card. Agents write their questions in
// markdown, and the cards showed it raw — `**Roughly how many?**` with the
// asterisks.
//
// Plain react-markdown and gfm, no reference chips: a chip resolves its id
// through chat-core's context and opens a session through the router, and the
// cards render with neither (see `SignalCard`). Raw HTML in the text is dropped,
// which is react-markdown's default, and so are `javascript:` links.

type MarkdownComponents = ComponentProps<typeof ReactMarkdown>['components'];

/** What inline text may keep. No links, and nothing that makes a block: this
 *  text sits inside an option's button or on a one-line title, where a nested
 *  link would steal the button's click and a paragraph would break the line. */
const INLINE_ELEMENTS = ['strong', 'em', 'del', 'code'];

/** Inline markdown is parsed as a paragraph like any other; the `p` is dropped
 *  and its children kept, so the text flows into whatever holds it. */
const INLINE_COMPONENTS: MarkdownComponents = {
  p: ({ children }) => <>{children}</>,
};

/** A card's body: full markdown, in a `div`, styled by the same
 *  `bc-turns-md` rules as the transcript. Memoized on its text, as is the inline
 *  form below: parsing is the costly part of drawing a card, and a card re-renders
 *  far more often than its text changes. */
export const SignalBodyMarkdown = memo(function SignalBodyMarkdown({ text }: { text: string }): JSX.Element {
  // The shared reader, so an id in a card is the same chip it is in the chat.
  return <ReferenceMarkdown text={text} className="signal-body bc-turns-md" />;
});

/** A title or an option description: emphasis and code only. Any other
 *  element is unwrapped to its text rather than dropped with it.
 *
 *  Line-start block markers are escaped first, because unwrapping keeps a
 *  list item's text but not its marker: `1. Which approach?` would lose the
 *  `1.`. Escaped, it stays text and every character survives. */
export const SignalInlineMarkdown = memo(function SignalInlineMarkdown({ text }: { text: string }): JSX.Element {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      allowedElements={[...INLINE_ELEMENTS, 'p']}
      unwrapDisallowed
      components={INLINE_COMPONENTS}
    >
      {escapeBlockMarkers(text)}
    </ReactMarkdown>
  );
});

/** Backslash-escape what would start a block at the head of a line — a
 *  heading `#`, a quote `>`, a bullet `-` `*` `+`, an ordered `1.` or `1)` —
 *  so inline rendering shows it as written. Exported for its test.
 *
 *  A heading or bullet marker counts only with a space after it, as in
 *  markdown itself; `**bold**` at the head of a line must stay bold. */
export function escapeBlockMarkers(text: string): string {
  return text
    .replace(/^(\s*)(>|[#+*-](?=\s|$))/gm, '$1\\$2')
    .replace(/^(\s*\d+)([.)](?=\s|$))/gm, '$1\\$2');
}

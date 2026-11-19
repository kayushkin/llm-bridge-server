import { createElement, type ReactElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import {
  ApiClient,
  ChatContext,
  groupSignalsByRequest,
  signalFromWire,
  type ChatContextValue,
  type Signal,
  type SignalRequest,
  type SignalWire,
} from '@kayushkin/chat-core'
import { SignalCard, SignalRequestCard } from '../src/components/chat/SignalCard'
import { ChatContext } from '@kayushkin/chat-core'
import { BridgeContext, DEFAULT_BRIDGE_ROUTES } from '../src/context'
import { MemoryRouter } from 'react-router-dom'

/** A card's body is drawn by the shared reference reader, which reads the
 *  page's config and the chat's lookups, as every page under <Bridge> has. */
function withPage(element: ReactElement): ReactElement {
  return createElement(MemoryRouter, null,
    createElement(BridgeContext.Provider, { value: { routes: DEFAULT_BRIDGE_ROUTES } as never },
      createElement(ChatContext.Provider, { value: { resolve: null, references: null } as never }, element)))
}
import { SignalRequestList } from '../src/components/chat/SessionSignals'
import { escapeBlockMarkers } from '../src/components/chat/signalMarkdown'

// What the signal cards DRAW, for a given record — moved here from chat-core's
// test/sessionSignals.test.ts on 2026-09-10 along with the cards. The verbs and
// reads those cards call are chat-core's and are still pinned there; these pin
// that a card never promises a choice the tool would refuse, and never hides
// the one way a question can be answered.

const BASE = '/api/bridge';

interface Call {
  url: string;
  method: string;
  body: unknown;
}

/** A `Response`-shaped object. Only the four members `ApiClient` touches. */
function respond(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: async () => body,
    text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
  } as unknown as Response;
}

/** A fetch that routes on the URL and records every call, so a test can assert
 *  BOTH what went on the wire and what did NOT (a decline must never fetch the
 *  pending hooks; a derived answer must never touch the resolve route). */
function recordingFetch(
  route: (url: string) => Response | undefined,
): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fn = async (url: string, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? 'GET';
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
    calls.push({ url: String(url), method, body });
    const res = route(String(url));
    if (!res) throw new Error(`no route for ${method} ${String(url)}`);
    return res;
  };
  return { fetch: fn as unknown as typeof fetch, calls };
}

function client(route: (url: string) => Response | undefined): { api: ApiClient; calls: Call[] } {
  const { fetch, calls } = recordingFetch(route);
  return { api: new ApiClient({ fetch, basePath: BASE }), calls };
}

function question(overrides: Partial<SignalWire>): Signal {
  return signalFromWire({
    id: 'sig-1',
    session_id: 'br_a',
    kind: 'question',
    source: 'tool',
    surface: 'chat',
    title: 'Which database?',
    state: 'open',
    created_at: '2026-08-17T10:00:00Z',
    ...overrides,
  });
}

/** A SignalRequestCard needs one thing from context — the ApiClient — and these
 *  cases never let it reach the wire, so a bare provider around it is the whole
 *  of the wiring. Deliberately NOT a ChatProvider: that builds a cache, a sync
 *  engine and a boot sequence to answer a question about a button. */
function inChatContext(node: ReactElement): ReactElement {
  const { api } = client(() => respond(200, {}));
  return createElement(ChatContext.Provider, { value: { api } as ChatContextValue }, node);
}

function requestOf(...signals: Signal[]): SignalRequest {
  const [request] = groupSignalsByRequest(signals);
  if (!request) throw new Error('no request grouped');
  return request;
}

describe('the signal cards render what the record says', () => {
  it('draws checkboxes for a pick-many question, and buttons for a pick-one', () => {
    const many = question({ id: 'sig-m', allow_multiple_options: true,
      options: [{ label: 'a.go', value: 'a.go' }, { label: 'b.go', value: 'b.go' }] });
    const one = question({ id: 'sig-o',
      options: [{ label: 'Yes', value: 'Yes' }, { label: 'No', value: 'No' }] });

    expect(renderToStaticMarkup(createElement(SignalCard, { signal: many })))
      .toContain('type="checkbox"');
    // A pick-one question answers ON the click, so it draws no radio anywhere.
    // A radio's whole job is to hold a choice until a Submit, and a card that
    // sends on the click has no Submit to hold it for.
    expect(renderToStaticMarkup(createElement(SignalCard, { signal: one })))
      .not.toContain('type="radio"');
    expect(renderToStaticMarkup(createElement(SignalCard, { signal: one })))
      .not.toContain('type="checkbox"');
    expect(renderToStaticMarkup(createElement(SignalCard, { signal: one })))
      .toContain('signal-option-pick');
    // And the pick-many form keeps its checkboxes to itself — a card drawing
    // both controls would be two opinions about the same question.
    expect(renderToStaticMarkup(createElement(SignalCard, { signal: many })))
      .not.toContain('signal-option-pick');
  });

  it('reads the flag off the record and never infers it from the options', () => {
    // Two options and no flag is a pick-one question. Guessing multi-ness from
    // the option count would let a human send back an answer the tool refuses,
    // with nothing on screen to say the extra choice was never allowed.
    const twoOptionsNoFlag = question({
      options: [{ label: 'a', value: 'a' }, { label: 'b', value: 'b' }],
    });
    expect(twoOptionsNoFlag.allowMultipleOptions).toBe(false);
    expect(renderToStaticMarkup(createElement(SignalCard, { signal: twoOptionsNoFlag })))
      .not.toContain('type="checkbox"');
  });

  it('renders nothing at all, rather than an empty box or an error', () => {
    // What the 404 leaves the surface holding: no signals, therefore no request
    // groups. `SignalRequestList` is the whole render path below `SessionSignals`
    // — asserting on it is what makes the empty case checkable without a DOM.
    const groups = groupSignalsByRequest([]);
    expect(renderToStaticMarkup(createElement(SignalRequestList, { requests: groups }))).toBe('');
  });

  it('renders a question with its options outside any provider', () => {
    const signal = question({
      id: 'sig-1',
      request_id: 'req-7',
      title: 'Which database?',
      options: [
        { label: 'Postgres', value: 'pg', description: 'already deployed' },
        { label: 'SQLite', value: '' },
      ],
    });
    // No ChatProvider anywhere. That is the point: the same card renders in the
    // raising session's chat, in a cross-session inbox, and inside ANOTHER
    // session's RefChip panel, because it never asks which session is active.
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal, answer: { pickedOptionValues: ['pg'] } }),
    );
    expect(html).toContain('Which database?');
    expect(html).toContain('Postgres');
    expect(html).toContain('already deployed');
    expect(html).toContain('signal-option-selected');
  });

  it('drops descriptions when compact, and still renders the options', () => {
    const signal = question({
      id: 'sig-1',
      request_id: 'req-7',
      allow_freeform: true,
      options: [{ label: 'Postgres', value: 'pg', description: 'already deployed' }],
    });
    const html = renderToStaticMarkup(createElement(SignalCard, { signal, compact: true }));
    // A compact card is still answerable — the question and its options stay.
    expect(html).toContain('Postgres');
    expect(html).not.toContain('already deployed');
    // Compact trims chrome, never the means of answering: the box stays.
    expect(html).toContain('textarea');
  });

  it('keeps the freeform box when compact and the question has no options', () => {
    // Both server producers mint exactly this: signal_classifier.go sets
    // AllowFreeform with options empty unless the assistant enumerated choices,
    // and signals.go copies a possibly-empty question.Options. the chat page's only
    // signals surface renders compact, so suppressing the box here left such a
    // question with no radios, no textarea, and a Submit that
    // `everyQuestionAnswered` disables forever — unanswerable anywhere.
    const signal = question({
      id: 'sig-1',
      request_id: 'req-7',
      allow_freeform: true,
      options: [],
    });
    const html = renderToStaticMarkup(createElement(SignalCard, { signal, compact: true }));
    expect(html).toContain('textarea');
    expect(html).toContain('Type your answer');
  });

  it('gives a question a freeform box even when allow_freeform is absent from the wire', () => {
    // signalFromWire defaults the flag to FALSE when the key is missing, so
    // honoring it meant an older row — or any producer that omits it — rendered
    // a question nobody could answer. Nothing sets it false on purpose: both
    // producers hardcode it true and the server never rejects a freeform answer.
    const signal = signalFromWire({
      id: 'sig-2',
      session_id: 'sess-1',
      kind: 'question',
      surface: 'chat',
      title: 'Which classifier transport?',
      state: 'open',
    } as SignalWire);
    expect(signal.allowFreeform).toBe(false);
    const html = renderToStaticMarkup(createElement(SignalCard, { signal }));
    expect(html).toContain('textarea');
  });

  it('never gives a notification a freeform box — it is acknowledged, not answered', () => {
    const signal = signalFromWire({
      id: 'sig-3',
      session_id: 'sess-1',
      kind: 'notification',
      surface: 'chat',
      title: 'Deploy finished',
      state: 'open',
    } as SignalWire);
    const html = renderToStaticMarkup(createElement(SignalCard, { signal }));
    expect(html).not.toContain('textarea');
  });
});

describe('an answer you can rewrite before you send it', () => {
  it('gives every option its own edit control', () => {
    const signal = question({
      options: [{ label: 'Yes', value: 'Yes' }, { label: 'No', value: 'No' }],
    });
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal, onChangeAnswer: () => {} }),
    );
    expect(html.match(/signal-option-edit"/g)).toHaveLength(2);
    // Named after what it rewrites, so a screen reader says which answer is
    // about to change rather than "button" — and named WITHOUT the word
    // "answer", or every pencil on the card answers to the Submit button's name.
    expect(html).toContain('Rewrite “Yes”');
    expect(html).not.toContain('before answering');
  });

  it('keeps the pencil OUT of the pick-many label', () => {
    // A <button> inside a <label> is activated by the label, so a pencil nested
    // there would tick the box it sits on every time it was clicked. Same reason
    // the pick-one control is a sibling and not a child.
    const many = question({
      allow_multiple_options: true,
      options: [{ label: 'a.go', value: 'a.go' }],
    });
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal: many, onChangeAnswer: () => {} }),
    );
    expect(html).toContain('</label><button type="button" class="signal-option-edit"');
  });
});

describe('a question that answers on the click', () => {
  // One click is the whole answer when a request holds a single pick-one
  // question, so the card sends it and draws no Submit. The three conditions are
  // each load-bearing, and the cases below are one per condition.

  it('draws no Submit for a lone pick-one question in the chat pane', () => {
    const one = question({ id: 'sig-1', request_id: 'req-1',
      options: [{ label: 'Yes', value: 'Yes' }, { label: 'No', value: 'No' }] });
    const html = renderToStaticMarkup(
      inChatContext(createElement(SignalRequestCard, {
        request: requestOf(one),
        startCollapsedToAnswers: true,
      })),
    );
    expect(html).not.toContain('signal-submit');
    expect(html).toContain('signal-option-pick');
  });

  it('keeps Submit for a pick-many question — it is not finished until you say so', () => {
    const many = question({ id: 'sig-1', request_id: 'req-1', allow_multiple_options: true,
      options: [{ label: 'a.go', value: 'a.go' }, { label: 'b.go', value: 'b.go' }] });
    const html = renderToStaticMarkup(
      inChatContext(createElement(SignalRequestCard, {
        request: requestOf(many),
        startCollapsedToAnswers: true,
      })),
    );
    expect(html).toContain('signal-submit');
  });

  it('keeps Submit when the request carries a second question', () => {
    // The request resolves ONCE, with every answer together. Sending on the
    // first click would resolve it with the rest blank.
    const first = question({ id: 'sig-1', request_id: 'req-1',
      options: [{ label: 'Yes', value: 'Yes' }] });
    const second = question({ id: 'sig-2', request_id: 'req-1', title: 'Which branch?',
      options: [{ label: 'main', value: 'main' }] });
    const html = renderToStaticMarkup(
      inChatContext(createElement(SignalRequestCard, {
        request: requestOf(first, second),
        startCollapsedToAnswers: true,
      })),
    );
    expect(html).toContain('signal-submit');
  });

  it('keeps Submit wherever a freeform box is still on the card', () => {
    // The sidebar surfaces keep their box, and text typed into it has nothing
    // else to send it — the options answer on the click, the box does not.
    const one = question({ id: 'sig-1', request_id: 'req-1',
      options: [{ label: 'Yes', value: 'Yes' }] });
    const html = renderToStaticMarkup(
      inChatContext(createElement(SignalRequestCard, { request: requestOf(one) })),
    );
    expect(html).toContain('textarea');
    expect(html).toContain('signal-submit');
  });
});

describe('a card opened showing only its answers', () => {
  // The chat pane's cards sit directly under the transcript that already carries
  // the question, so repeating it there is the same words twice.

  it('hides the question and its summary behind a disclosure', () => {
    const signal = question({
      title: 'Which database?',
      body: 'Postgres is already deployed; SQLite would be new.',
      options: [{ label: 'Postgres', value: 'pg', description: 'already deployed' }],
    });
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal, startCollapsedToAnswers: true }),
    );
    expect(html).not.toContain('Which database?');
    expect(html).not.toContain('already deployed');
    // The answers are what is left, and the control that puts the rest back is
    // named for what it does.
    expect(html).toContain('Postgres');
    expect(html).toContain('Show the question');
  });

  it('drops the freeform box when there are options to rewrite instead', () => {
    const signal = question({ options: [{ label: 'Yes', value: 'Yes' }] });
    expect(
      renderToStaticMarkup(createElement(SignalCard, { signal, startCollapsedToAnswers: true })),
    ).not.toContain('textarea');
  });

  it('shows only the title of a question with no options — the composer answers it', () => {
    // A derived question is answered by the session's next message, and the
    // composer is right below the card. A tool question always has options.
    // The title stays so the card still says what it asks; the summary waits
    // behind the disclosure.
    const signal = question({
      title: 'Make that edit?',
      body: 'It touches the deploy script.',
      options: [],
    });
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal, startCollapsedToAnswers: true }),
    );
    expect(html).not.toContain('textarea');
    expect(html).toContain('Make that edit?');
    expect(html).not.toContain('It touches the deploy script.');
  });

  it('draws no Answer button for a question with nothing on the card to answer with', () => {
    // A disabled button that can never enable is noise.
    const signal = question({ id: 'sig-1', options: [] });
    const html = renderToStaticMarkup(
      inChatContext(createElement(SignalRequestCard, {
        request: requestOf(signal),
        startCollapsedToAnswers: true,
      })),
    );
    expect(html).not.toContain('signal-submit');
  });

  it('keeps the freeform box on a question with no options everywhere else', () => {
    // The inbox and the kanban drawer have no composer below them.
    const signal = question({ options: [] });
    expect(renderToStaticMarkup(createElement(SignalCard, { signal }))).toContain('textarea');
  });

  it('shows a notification as its Acknowledge button, with the text behind the disclosure', () => {
    // The transcript above already says what the notification says.
    const note = question({ kind: 'notification', title: 'The deploy finished' });
    const html = renderToStaticMarkup(
      createElement(SignalCard, {
        signal: note,
        startCollapsedToAnswers: true,
        onAcknowledge: () => {},
      }),
    );
    expect(html).not.toContain('The deploy finished');
    expect(html).toContain('Show the notification');
    expect(html).toContain('signal-ack');
  });

  it('shows a notification in full on a surface that does not collapse', () => {
    const note = question({ kind: 'notification', title: 'The deploy finished' });
    const html = renderToStaticMarkup(
      createElement(SignalCard, { signal: note, onAcknowledge: () => {} }),
    );
    expect(html).toContain('The deploy finished');
    expect(html).not.toContain('signal-disclosure');
    expect(html).toContain('signal-ack');
  });
});


describe('a card renders its text as markdown', () => {
  // Agents write their questions in markdown; the cards used to show the
  // asterisks.

  it('renders the body as markdown, and opens its links in a new tab', () => {
    const signal = question({
      body: '**Roughly how many?**\n\n- one\n- two\n\nSee [the doc](https://example.com).',
    });
    const html = renderToStaticMarkup(withPage(createElement(SignalCard, { signal })));
    expect(html).toContain('<strong>Roughly how many?</strong>');
    expect(html).toContain('<li>one</li>');
    expect(html).not.toContain('**');
    expect(html).toContain('href="https://example.com" target="_blank" rel="noopener noreferrer"');
  });

  it('drops raw HTML and script links from the body', () => {
    const signal = question({ body: '<img src=x onerror=alert(1)> [x](javascript:alert(1))' });
    const html = renderToStaticMarkup(withPage(createElement(SignalCard, { signal })));
    expect(html).not.toContain('<img');
    expect(html).not.toContain('javascript:');
  });

  it('keeps a title and an option description inline, with no links or blocks', () => {
    const signal = question({
      title: '1. Use **Postgres** or [SQLite](https://example.com)?',
      options: [{ label: 'Postgres', value: 'pg', description: '`pg` is *already* deployed' }],
    });
    const html = renderToStaticMarkup(createElement(SignalCard, { signal }));
    // The "1." survives: unwrapping a list would keep the text and drop it.
    expect(html).toContain('1. Use <strong>Postgres</strong> or SQLite?');
    expect(html).not.toContain('<a ');
    expect(html).not.toContain('<ol');
    expect(html).toContain('<code>pg</code> is <em>already</em> deployed');
  });

  it('leaves the option label as plain text', () => {
    // The label is also what the rewrite box opens with, so it shows as written.
    const signal = question({ options: [{ label: '**Yes**', value: 'yes' }] });
    const html = renderToStaticMarkup(createElement(SignalCard, { signal }));
    expect(html).toContain('**Yes**');
  });
});

describe('escapeBlockMarkers', () => {
  it('escapes line-start markers that would make a block', () => {
    expect(escapeBlockMarkers('# a\n> b\n- c\n* d\n+ e\n1. f\n2) g')).toBe(
      '\\# a\n\\> b\n\\- c\n\\* d\n\\+ e\n1\\. f\n2\\) g',
    );
  });

  it('leaves emphasis and plain numbers at the start of a line alone', () => {
    expect(escapeBlockMarkers('**bold** first')).toBe('**bold** first');
    expect(escapeBlockMarkers('*it* first')).toBe('*it* first');
    expect(escapeBlockMarkers('#hashtag')).toBe('#hashtag');
    expect(escapeBlockMarkers('2026.09 release')).toBe('2026.09 release');
  });
});

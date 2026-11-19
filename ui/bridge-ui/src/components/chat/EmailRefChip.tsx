import { useEffect, useState, type JSX } from 'react';
import { useNavigate } from 'react-router-dom';
import { useBridgeConfig } from '../../context';
import type { FetchFn } from '../../types';
import { parseEmailLocator } from '../../kanbanAxes';
import type { MailMessage } from '../../types-mailstack';
import { AnchoredPanel, RefRow, idTail, timeAgo, truncate, useAnchoredPanel } from './RefChip';

/**
 * A reference chip for one email, named by its mailstack locator
 * (`<account_id>:<provider message id>`, the ref an `email` card link and a
 * ticket's source carry). The label is the message's subject; the panel shows
 * sender, date and snippet, and "Open in Mail" deep-links to the host's mail
 * page.
 *
 * Unlike {@link RefChip} this reads the bridge config, not the chat context,
 * because mailstack is reached through the host's `mailBasePath`. A host that
 * carries no mail gets the locator as a plain chip with nothing to load.
 *
 * mailstack fetches each message from its provider live, so a chip loads its
 * message once per page load and every chip for the same locator shares that
 * one request.
 */
export function EmailRefChip({ locator, className }: { locator: string; className?: string }): JSX.Element {
  const { fetch: fetchFn, mailBasePath, mailPagePath } = useBridgeConfig();
  const navigate = useNavigate();
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const parsed = parseEmailLocator(locator);
  const [message, setMessage] = useState<MailMessage | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!parsed || !mailBasePath) return;
    let canceled = false;
    loadMessage(fetchFn, mailBasePath, locator).then(
      (loaded) => { if (!canceled) setMessage(loaded); },
      (failure: unknown) => { if (!canceled) setError(String(failure)); },
    );
    return () => { canceled = true; };
    // parsed is derived from locator, so locator is the dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fetchFn, mailBasePath, locator]);

  const subject = message?.meta?.subject;
  const label = subject ? truncate(subject, 40) : parsed ? idTail(parsed.messageID) : locator;
  const sender = message?.meta?.from?.name || message?.meta?.from?.email;

  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="email" data-ref-id={locator}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-email${open ? ' ref-chip-open' : ''}${error ? ' ref-chip-error' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={error ? `email ${locator} — couldn’t load: ${error}` : `email — ${subject || locator}`}
      >
        {/* A message that would not load says so on the chip itself, so a
            table of chips shows which failed without opening each one. */}
        <span className="ref-chip-glyph" aria-hidden>{error ? '⚠' : '✉'}</span>
        <span className="ref-chip-label">{label}</span>
        <span className="ref-chip-caret-inline" aria-hidden>▾</span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="email details" refId={locator} refKind="email">
          {!parsed && <div className="ref-chip-panel-error">Not a mailstack locator: {locator}</div>}
          {parsed && !mailBasePath && <div className="ref-chip-panel-error">This host carries no mail service.</div>}
          {parsed && mailBasePath && !message && !error && <div className="ref-chip-panel-loading">Loading email…</div>}
          {/* Reported rather than hidden: a message can be gone upstream, or its
              account can have lost its credentials, and a chip that quietly
              shows an id would read as "this email has no subject". */}
          {error && <div className="ref-chip-panel-error">Couldn’t load email: {error}</div>}
          {message && <div className="ref-chip-panel-title">{subject || '(no subject)'}</div>}
          {sender && <RefRow label="From" value={sender} />}
          {message?.meta?.date && <RefRow label="Sent" value={timeAgo(message.meta.date)} />}
          {parsed && <RefRow label="Account" value={parsed.accountID} />}
          {message?.meta?.snippet && <div className="ref-chip-item-body">{message.meta.snippet}</div>}
          {parsed && mailPagePath && (
            <div className="ref-chip-panel-actions">
              <button
                type="button"
                className="ref-chip-panel-btn"
                onClick={() => navigate(
                  `${mailPagePath}?account=${encodeURIComponent(parsed.accountID)}&message=${encodeURIComponent(parsed.messageID)}`,
                )}
              >
                Open in Mail ↗
              </button>
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  );
}

/** One request per locator per page load, shared by every chip that names it.
 *  A failure is dropped from the cache so the next mount asks again. */
const messagesByLocator = new Map<string, Promise<MailMessage>>();

function loadMessage(fetchFn: FetchFn, mailBasePath: string, locator: string): Promise<MailMessage> {
  const key = `${mailBasePath}|${locator}`;
  const cached = messagesByLocator.get(key);
  if (cached) return cached;
  const parsed = parseEmailLocator(locator);
  if (!parsed) return Promise.reject(new Error(`not a mailstack locator: ${locator}`));
  const request = (async () => {
    const res = await fetchFn(
      `${mailBasePath}/messages/${encodeURIComponent(parsed.messageID)}?account=${encodeURIComponent(parsed.accountID)}`,
    );
    const body = await res.json().catch(() => null);
    if (!res.ok) throw new Error(body?.error ? `HTTP ${res.status}: ${body.error}` : `HTTP ${res.status}`);
    return body as MailMessage;
  })();
  messagesByLocator.set(key, request);
  request.catch(() => messagesByLocator.delete(key));
  return request;
}

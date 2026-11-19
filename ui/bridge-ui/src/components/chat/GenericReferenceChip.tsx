// The chip for a record whose type has no chip of its own, drawn from the
// type's row in reference-store's list: its glyph and name on the chip, and in
// the panel what the type is, the summary fields its row names, and a link to
// its page (or the generic record page). And the chip for a record the
// signed-in person may not see, which shows only that. Rules in
// src/referenceChips.ts.

import type { JSX } from 'react'
import { useReferenceTypes, type ResolvedRefMatch } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import {
  isOffSiteHref,
  referenceChipLabel,
  referencePageHref,
  referenceSummaryRows,
} from '../../referenceChips'
import { AnchoredPanel, RefChip, RefRow, truncate, useAnchoredPanel } from './RefChip'
import { useRefChipServer } from './refChipServer'

export function GenericReferenceChip({
  refId,
  match,
  className,
}: {
  refId: string
  match: ResolvedRefMatch
  className?: string
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel()
  const types = useReferenceTypes()
  const { routes } = useBridgeConfig()
  const server = useRefChipServer()
  const type = types?.get(match.type)
  const label = referenceChipLabel(match, refId)
  const rows = referenceSummaryRows(type, match.data)
  // A page link from a connected server names that server's page, which this
  // site does not serve, so a chip there links nowhere.
  const href = server ? '' : referencePageHref(match, refId, routes.reference)
  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind={match.type} data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={label === refId ? refId : `${label} (${refId})`}
      >
        <span className="ref-chip-glyph" aria-hidden>
          {type?.glyph ?? '🔗'}
        </span>
        <span className="ref-chip-label">{truncate(label)}</span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="Reference details" refId={refId} refKind={match.type}>
          <div className="ref-chip-panel-title">{label}</div>
          {type?.description && <div className="ref-chip-panel-loading">{type.description}</div>}
          <RefRow label="Id" value={refId} />
          {rows.map((row) =>
            row.format === 'reference' ? (
              <div className="ref-chip-panel-row" key={row.label}>
                <span className="ref-chip-panel-label">{row.label}</span>
                <span className="ref-chip-panel-value">
                  <RefChip kind="uuid" refId={row.text} />
                </span>
              </div>
            ) : row.format === 'url' ? (
              <div className="ref-chip-panel-row" key={row.label}>
                <span className="ref-chip-panel-label">{row.label}</span>
                <span className="ref-chip-panel-value">
                  <a href={row.text} target="_blank" rel="noreferrer">
                    {truncate(row.text, 48)}
                  </a>
                </span>
              </div>
            ) : (
              <RefRow key={row.label} label={row.label || 'About'} value={row.text} />
            ),
          )}
          {href && (
            <div className="ref-chip-panel-actions">
              <a
                className="ref-chip-panel-btn"
                href={href}
                {...(isOffSiteHref(href) ? { target: '_blank', rel: 'noreferrer' } : {})}
              >
                Open
              </a>
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  )
}

/** A record that exists and that the signed-in person may not see: the id
 *  and its type, and nothing about the record. */
export function RefusedReferenceChip({ refId, match }: { refId: string; match: ResolvedRefMatch }): JSX.Element {
  const types = useReferenceTypes()
  const type = types?.get(match.type)
  return (
    <span
      className="ref-chip ref-chip-item ref-chip-refused"
      data-ref-kind={match.type}
      data-ref-id={refId}
      title={`A ${match.type.replace(/_/g, ' ')} you do not have access to`}
    >
      <span className="ref-chip-glyph" aria-hidden>
        🔒
      </span>
      <span className="ref-chip-label">
        {type?.glyph ? `${type.glyph} ` : ''}
        {refId}
      </span>
    </span>
  )
}

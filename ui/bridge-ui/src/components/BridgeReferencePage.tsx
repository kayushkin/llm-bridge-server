// The generic record page, `/ref/<id>`: any record reference-store can look up
// for the signed-in person, for a chip whose type has no page of its own. It
// shows what the id names — each record if several stores hold it — with the
// type's summary, every top-level value the store sent, and a link to the
// record's own page when its type has one. Rules in src/referenceChips.ts.

import type { JSX } from 'react'
import { useParams } from 'react-router-dom'
import { useReferenceTypes, useResolvedRef, type ResolvedRefMatch } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../context'
import {
  isOffSiteHref,
  referenceChipLabel,
  referenceRecordFields,
  referenceSummaryRows,
} from '../referenceChips'
import { RefChip } from './chat/RefChip'

export function BridgeReferencePage(): JSX.Element {
  const { refId = '' } = useParams<{ refId: string }>()
  const { referenceStoreBasePath } = useBridgeConfig()
  const { data: matches, error, loading } = useResolvedRef(refId)

  if (!referenceStoreBasePath) {
    return <div className="bridge-page bc-reference-page"><p>reference-store is not configured on this site, so no record can be looked up here.</p></div>
  }
  return (
    <div className="bridge-page bc-reference-page">
      <h1 className="bc-reference-page-id">{refId}</h1>
      {loading && <p>Looking it up…</p>}
      {error && <p className="bc-reference-page-error">Could not look it up: {error}</p>}
      {matches && matches.length === 0 && <p>Nothing on this server has that id.</p>}
      {matches?.map((match) => <ReferenceRecord key={`${match.service}/${match.type}`} refId={refId} match={match} />)}
    </div>
  )
}

function ReferenceRecord({ refId, match }: { refId: string; match: ResolvedRefMatch }): JSX.Element {
  const types = useReferenceTypes()
  const type = types?.get(match.type)
  if (match.refused) {
    return (
      <section className="bc-reference-record bc-reference-record-refused">
        <h2>🔒 A {match.type.replace(/_/g, ' ')}</h2>
        <p>You do not have access to this record. An administrator can grant it in Grants: can_see_references on this record or on its type.</p>
      </section>
    )
  }
  const rows = referenceSummaryRows(type, match.data)
  const fields = referenceRecordFields(match.data)
  return (
    <section className="bc-reference-record">
      <h2>
        <span aria-hidden>{type?.glyph ?? '🔗'} </span>
        {referenceChipLabel(match, refId)}
      </h2>
      <p className="bc-reference-record-type">
        {match.type.replace(/_/g, ' ')} · {match.service}
        {type?.description ? ` — ${type.description}` : ''}
      </p>
      {match.page_url && (
        <p>
          <a href={match.page_url} {...(isOffSiteHref(match.page_url) ? { target: '_blank', rel: 'noreferrer' } : {})}>
            Open its page
          </a>
        </p>
      )}
      {rows.length > 0 && (
        <dl className="bc-reference-record-summary">
          {rows.map((row) => (
            <div key={row.label}>
              <dt>{row.label || 'About'}</dt>
              <dd>{row.format === 'reference' ? <RefChip kind="uuid" refId={row.text} /> : row.text}</dd>
            </div>
          ))}
        </dl>
      )}
      {fields.length > 0 && (
        <details className="bc-reference-record-fields">
          <summary>Every value the store holds</summary>
          <table>
            <tbody>
              {fields.map(({ field, text }) => (
                <tr key={field}>
                  <th scope="row">{field}</th>
                  <td>{text}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </section>
  )
}

// The Bundles page's composer form, as data: what the form holds while a
// bundle is being written, and how that becomes the body `POST /bundles`
// takes. Pure — no React, no fetch — so the rules (what is refused, how tags
// are split, that a member is an id) are testable on their own.
//
// A draft keeps every field as the text the user typed; `bundleDraftToWire`
// is the one place it is parsed and checked, and it refuses rather than
// guesses: a member without a positive id would be a name join, and
// bundle-store rejects one at the write anyway.

import type { Bundle, Member as BundleMember, MemberKind as BundleMemberKind } from '@kayushkin/bundle-store-types'

export interface BundleDraftMember {
  kind: BundleMemberKind
  /** The owning store's id, as text — what a picker hands over. */
  id: string
  /** Display label, carried beside the id and never used to find it. */
  name: string
  /** A task tag that gates the member; empty means always included. */
  condition: string
  /** `include` or `deny`, as the store sent it. Carried through untouched: the
   *  form offers no way to change it, and dropping it would turn every denied tool
   *  into an included one on the next save, because the store reads an empty
   *  effect as `include`. Empty for a member added in this form, which is that
   *  same default. */
  effect: string
}

export interface BundleDraft {
  /** The unique name and the upsert key: writing a draft whose name is
   *  already a bundle's overwrites that bundle, members included. */
  name: string
  displayName: string
  description: string
  /** The parent bundle's id as text; empty means a root bundle. */
  extendsID: string
  /** Comma-separated, as typed. */
  matchTagsText: string
  model: string
  effort: string
  enabled: boolean
  members: BundleDraftMember[]
  /** Path patterns a session given this bundle may not read, as the store sent
   *  them. The form does not edit them yet, but it must send them back: the
   *  store's update overwrites the column, so leaving them out of a save would
   *  erase them. */
  deniedReadPaths: string[]
}

/** The body `POST /bundles` takes: a bundle without the fields the store
 *  assigns. `extends` (the parent's name) is left out on purpose — the store
 *  reads it back off the parent by id, so sending both would only give the
 *  two a way to disagree. */
export type BundleWrite = Pick<Bundle, 'name' | 'display_name' | 'description' | 'match_tags' | 'members' | 'denied_read_paths' | 'model' | 'effort' | 'enabled'>
  & { extends_id?: number }

export type BundleDraftResult = { ok: true; value: BundleWrite } | { ok: false; error: string }

export function emptyBundleDraft(): BundleDraft {
  return {
    name: '', displayName: '', description: '', extendsID: '', matchTagsText: '',
    model: '', effort: '', enabled: true, members: [], deniedReadPaths: [],
  }
}

/** A draft prefilled from a stored bundle, for editing it in place. */
export function bundleDraftOf(bundle: Bundle): BundleDraft {
  return {
    name: bundle.name,
    displayName: bundle.display_name ?? '',
    description: bundle.description ?? '',
    extendsID: bundle.extends_id ? String(bundle.extends_id) : '',
    matchTagsText: (bundle.match_tags ?? []).join(', '),
    model: bundle.model ?? '',
    effort: bundle.effort ?? '',
    enabled: bundle.enabled,
    members: (bundle.members ?? []).map(m => ({ kind: m.kind, id: String(m.id), name: m.name ?? '', condition: m.condition ?? '', effect: m.effect })),
    deniedReadPaths: bundle.denied_read_paths ?? [],
  }
}

/** Comma-separated tags: trimmed, empties dropped, first occurrence kept. */
export function parseTagList(text: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of text.split(',')) {
    const tag = raw.trim()
    if (!tag || seen.has(tag)) continue
    seen.add(tag)
    out.push(tag)
  }
  return out
}

const POSITIVE_INTEGER = /^[1-9][0-9]*$/

export function bundleDraftToWire(draft: BundleDraft): BundleDraftResult {
  const name = draft.name.trim()
  if (!name) return { ok: false, error: 'a bundle needs a name — it is the key the store upserts on' }
  if (/\s/.test(name)) return { ok: false, error: `bundle name ${JSON.stringify(name)} has whitespace in it; names are single tokens like "go-service"` }

  const members: BundleMember[] = []
  const seen = new Set<string>()
  for (const m of draft.members) {
    const idText = m.id.trim()
    if (!POSITIVE_INTEGER.test(idText)) {
      return { ok: false, error: `${m.kind} ${m.name ? JSON.stringify(m.name) + ' ' : ''}has no id — a member is the owning store's id, and ${JSON.stringify(idText)} is not one` }
    }
    const key = `${m.kind}:${idText}`
    if (seen.has(key)) return { ok: false, error: `${m.kind} #${idText} is on the list twice` }
    seen.add(key)
    const member: BundleMember = { kind: m.kind, id: Number(idText), effect: m.effect }
    if (m.name.trim()) member.name = m.name.trim()
    if (m.condition.trim()) member.condition = m.condition.trim()
    members.push(member)
  }

  const value: BundleWrite = {
    name,
    display_name: draft.displayName.trim(),
    description: draft.description.trim(),
    match_tags: parseTagList(draft.matchTagsText),
    members,
    denied_read_paths: draft.deniedReadPaths,
    model: draft.model.trim(),
    effort: draft.effort.trim(),
    enabled: draft.enabled,
  }
  const extendsText = draft.extendsID.trim()
  if (extendsText) {
    if (!POSITIVE_INTEGER.test(extendsText)) return { ok: false, error: `extends ${JSON.stringify(extendsText)} is not a bundle id` }
    value.extends_id = Number(extendsText)
  }
  return { ok: true, value }
}

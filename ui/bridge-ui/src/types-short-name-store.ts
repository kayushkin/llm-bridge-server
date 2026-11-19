// short-name-store's wire types, written by hand.
//
// Every other store's types come from its own generated package; short-name-store
// has none yet (no `ts/` directory and no tygo config), so there is nothing to
// import. These mirror `Namespace`, `ShortName` and `Resolution` in
// ~/repos/short-name-store/model.go and its CONTRACT.md. When the store renders
// its own package, import it and delete this file.

/** A page that has short names under it. */
export interface ShortNameNamespace {
  name: string
  description: string
  /** First segments no short name may start with: the page's own sub-pages. */
  reserved_segments: string[] | null
  created_at: number
  updated_at: number
}

/** One name that reaches one record. */
export interface ShortName {
  /** `short_name_000001`. */
  id: string
  namespace: string
  /** One to three segments joined by `/`. */
  path: string
  /** What kind of record it points at: `multichat_room`. */
  entity_type: string
  /** The owning store's id for the record, verbatim: a Matrix room id. */
  entity_id: string
  /** `operator`, or the command that chose it. */
  created_by: string
  reason: string
  created_at: number
  /** 0 and "" while the name is current. */
  superseded_at: number
  superseded_by: string
}

/** `GET /short-names/resolve`: the name asked for and the record's current
 *  name, the same row when it is current. */
export interface ShortNameResolution {
  short_name: ShortName
  current: ShortName
}

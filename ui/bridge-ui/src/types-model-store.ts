// model-store's wire types, written by hand.
//
// Every other store's types come from its own generated package; model-store
// has none yet — no `ts/` directory and no tygo config — so there is nothing
// to import. These mirror `ms.Model`/`ms.ModelStatus` in
// ~/repos/model-store/store.go and the `/api/roles` answer built in
// ~/repos/model-store/cmd/ms/serve.go. When model-store renders its own
// package, import it and delete this file.

/** One model in model-store's registry, as `GET /api/models` lists it. */
export interface ModelStoreModel {
  id: string
  /** The provider's id, e.g. "anthropic". */
  provider: string
  /** Display name. */
  name: string
  short_name: string
  aliases: string[] | null
  max_tokens: number
  /** US dollars per million input tokens. */
  input_cost: number
  /** US dollars per million output tokens. */
  output_cost: number
  enabled: boolean
  priority: number
}

/** `GET /api/roles`. */
export interface ModelStoreRoles {
  /** Each role's first model: the one a call uses. */
  roles: Record<string, string>
  /** Each role's whole list, first model first; the rest are tried in order
   *  when a call fails. */
  models: Record<string, string[]>
  /** The role names model-store knows, in its order. */
  canonical: string[]
}

/** `POST /api/roles` with a whole list. */
export interface ModelStoreRoleModelsBody {
  role: string
  models: string[]
}

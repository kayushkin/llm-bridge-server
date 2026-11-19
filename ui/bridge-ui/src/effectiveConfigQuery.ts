/** The dry-run inputs the effective-config page takes, and how they travel in
 *  the URL: `?harness=…&instance_id=…&principal_id=…&agent_id=…&bundle_id=…
 *  &board_id=…&card_id=…&tag=…&tag=…`. The same names llm-bridge-server's
 *  `GET /effective-config` reads, so the page's URL is the request. */
export interface EffectiveConfigInputs {
  harness: string
  instanceId: string
  principalId: string
  agentId: string
  bundleId: string
  boardId: string
  cardId: string
  tags: string[]
}

export const EMPTY_EFFECTIVE_CONFIG_INPUTS: EffectiveConfigInputs = {
  harness: '', instanceId: '', principalId: '', agentId: '', bundleId: '', boardId: '', cardId: '', tags: [],
}

const WIRE: Array<[keyof EffectiveConfigInputs, string]> = [
  ['harness', 'harness'], ['instanceId', 'instance_id'], ['principalId', 'principal_id'], ['agentId', 'agent_id'],
  ['bundleId', 'bundle_id'], ['boardId', 'board_id'], ['cardId', 'card_id'],
]

/** The query string for a dry run, without the leading `?`. Empty inputs are
 *  omitted; an empty result means there is nothing to ask yet. */
export function effectiveConfigQuery(inputs: EffectiveConfigInputs): string {
  const params = new URLSearchParams()
  for (const [field, wire] of WIRE) {
    const value = inputs[field]
    if (typeof value === 'string' && value !== '') params.set(wire, value)
  }
  for (const tag of inputs.tags) if (tag.trim()) params.append('tag', tag.trim())
  return params.toString()
}

export function effectiveConfigInputsFromParams(params: URLSearchParams): EffectiveConfigInputs {
  const out: EffectiveConfigInputs = { ...EMPTY_EFFECTIVE_CONFIG_INPUTS, tags: params.getAll('tag') }
  for (const [field, wire] of WIRE) {
    const value = params.get(wire)
    if (value && field !== 'tags') out[field] = value
  }
  return out
}

/** The server can answer when it knows the harness, or can take it from an
 *  instance or from a board's default instance. */
export function effectiveConfigInputsAreAskable(inputs: EffectiveConfigInputs): boolean {
  return inputs.harness !== '' || inputs.instanceId !== '' || inputs.boardId !== ''
}

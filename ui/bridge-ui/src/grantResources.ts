// The resources a grant names — the agents, harness instances, environments,
// skills and tools grant-store holds against a person or a group — and the
// pure helpers that turn those id rows into something a person can read.
//
// grant-store keeps ids only, each the id its owner assigned: agent-store's
// numeric `agents.id` (not the slug, which agent-store lets you rename),
// skill-store's and tool-store's numeric ids, and harness-store's instance and
// machine ids. Names are resolved here, live, from those owners, so a rename
// shows everywhere at once and no copied name can go stale.
//
// Whether a grant is a lock depends on its relation: `GET /relations` says
// which ones llm-bridge-server enforces at session start. `works_with` is not
// one of them — it is the list the card's "runs on" picker reads to offer an
// assignee's instances first.

import type { Instance } from '@kayushkin/llm-bridge-types'
import type { Machine } from './types'
import type { Grant } from '@kayushkin/grant-store-types'
import type { GrantResourceType } from './grantStoreClient'

export interface ResourceTypeWording {
  /** Section heading: "Harness instances". */
  plural: string
  /** In running text: "Add a harness instance…". */
  singular: string
  /** The store that owns the ids, named when a lookup fails. */
  owner: string
}

const WORDING: Record<GrantResourceType, ResourceTypeWording> = {
  agent: { plural: 'Agents', singular: 'agent', owner: 'agent-store' },
  instance: { plural: 'Harness instances', singular: 'harness instance', owner: 'harness-store' },
  // harness-store calls these machines; the operator calls them environments.
  machine: { plural: 'Environments', singular: 'environment', owner: 'harness-store' },
  skill: { plural: 'Skills', singular: 'skill', owner: 'skill-store' },
  tool: { plural: 'Tools', singular: 'tool', owner: 'tool-store' },
}

/** The words for a resource type. The vocabulary itself comes from
 *  `GET /resource-types`; only the wording lives here, and a type this library
 *  has no words for is shown under its own name rather than dropped. */
export function resourceTypeWording(type: string): ResourceTypeWording {
  const known = (WORDING as Record<string, ResourceTypeWording | undefined>)[type]
  return known ?? { plural: type, singular: type, owner: 'its owning store' }
}

/** One resource as a picker or a list row shows it. */
export interface ResourceOption {
  /** The owner's id, as text — principal-store keys numeric ids as text too. */
  id: string
  /** The owner's current name for it. */
  label: string
  /** One line of context: an instance's harness and environment, a skill's
   *  source. Empty when there is none. */
  detail: string
  /** The owner has it switched off. It can still be on a list — a disabled
   *  instance may come back — but it is flagged. */
  disabled: boolean
}

/** The part of agent-store's record a row needs. */
export interface AgentRecord {
  id: number
  slug: string
  display_name: string
  emoji?: string
  enabled: boolean
}

/** The part of skill-store's record a row needs. */
export interface SkillRecord {
  id: number
  name: string
  source_name: string
  enabled: boolean
}

/** The part of tool-store's record a row needs. tool-store omits
 *  `display_name` when it is empty (`omitempty`), and most tools have none. */
export interface ToolRecord {
  id: number
  name: string
  display_name?: string
  kind: string
  enabled: boolean
}

export function machineLabel(machine: Pick<Machine, 'name' | 'emoji'>): string {
  return machine.emoji ? `${machine.emoji} ${machine.name}` : machine.name
}

export function agentOption(agent: AgentRecord): ResourceOption {
  const label = agent.emoji ? `${agent.emoji} ${agent.display_name}` : agent.display_name
  return { id: String(agent.id), label, detail: agent.slug, disabled: !agent.enabled }
}

/** An instance names its harness and the environment it runs in, because two
 *  instances routinely share a name ("SSDawn" is both a claude_code and an
 *  inber instance). A machine the list does not know shows as its raw id. */
export function instanceOption(instance: Instance, machinesByID: ReadonlyMap<string, Machine>): ResourceOption {
  const machine = machinesByID.get(instance.machine_id)
  return {
    id: instance.id,
    label: instance.name,
    detail: `${instance.harness_type} · ${machine ? machineLabel(machine) : instance.machine_id}`,
    disabled: !instance.enabled,
  }
}

export function machineOption(machine: Machine): ResourceOption {
  return { id: machine.id, label: machineLabel(machine), detail: machine.hostname ?? '', disabled: false }
}

export function skillOption(skill: SkillRecord): ResourceOption {
  return { id: String(skill.id), label: skill.name, detail: skill.source_name, disabled: !skill.enabled }
}

/** Labeled by `name`, which every tool has, with `display_name` beside the
 *  kind when there is one — the Bundles composer's wording. */
export function toolOption(tool: ToolRecord): ResourceOption {
  const detail = tool.display_name && tool.display_name !== tool.name ? `${tool.kind} · ${tool.display_name}` : tool.kind
  return { id: String(tool.id), label: tool.name, detail, disabled: !tool.enabled }
}

export function compareOptionsByLabel(a: ResourceOption, b: ResourceOption): number {
  return a.label.localeCompare(b.label, undefined, { sensitivity: 'base' }) || a.id.localeCompare(b.id)
}

/** Options whose label, detail or id contains the query, case-insensitively,
 *  minus the ids already on the list. */
export function filterResourceOptions(
  options: readonly ResourceOption[], query: string, excludeIDs: ReadonlySet<string>,
): ResourceOption[] {
  const needle = query.trim().toLowerCase()
  return options.filter(option =>
    !excludeIDs.has(option.id)
    && (!needle || `${option.label} ${option.detail} ${option.id}`.toLowerCase().includes(needle)))
}

/** One relation's rows of one type for one principal, split into the ones the
 *  principal holds itself and the ones it inherits from a group. grant-store
 *  marks the difference with the row's `principal_id`: the principal asked
 *  about, or the group the grant belongs to. */
export function partitionGrantRows(
  rows: readonly Grant[], principalID: string, relation: string, resourceType: string,
): { direct: Grant[]; inherited: Grant[] } {
  const direct: Grant[] = []
  const inherited: Grant[] = []
  for (const row of rows) {
    if (row.relation !== relation || row.resource_type !== resourceType) continue
    if (row.principal_id === principalID) direct.push(row)
    else inherited.push(row)
  }
  return { direct, inherited }
}

/** The words for a relation, as a section heading. The vocabulary itself comes
 *  from `GET /relations`; only the wording lives here, and a relation this
 *  library has no words for is shown under its own name rather than dropped. */
export function relationWording(relation: string): string {
  const known: Record<string, string> = {
    can_use: 'May use',
    can_run_as: 'May run as',
    can_dispatch_on: 'May dispatch on',
    works_with: 'Works with',
  }
  return known[relation] ?? relation
}

// --- the card's "runs on" picker -------------------------------------------

export interface DispatchInstanceChoice {
  instance: Instance
  /** The environment's label, or the raw machine id when the machine list does
   *  not know it. */
  machineLabel: string
  /** The card assignees whose list carries this instance, in the order given. */
  listedBy: string[]
}

export interface DispatchInstanceChoices {
  /** Instances on an assignee's list, offered first. */
  listed: DispatchInstanceChoice[]
  /** Every other enabled instance, grouped by the environment it runs in. */
  byMachine: { machineID: string; machineLabel: string; choices: DispatchInstanceChoice[] }[]
}

/**
 * What the card's "runs on" select offers. Only enabled instances: the server
 * answers 503 for a disabled one, so offering it would only defer the refusal.
 * An instance on an assignee's list appears once, in the first group, and not
 * again under its environment — one select with two options of the same value
 * reads as two different choices.
 */
export function dispatchInstanceChoices(
  instances: readonly Instance[],
  machines: readonly Machine[],
  listedBy: ReadonlyMap<string, string[]>,
): DispatchInstanceChoices {
  const machinesByID = new Map(machines.map(machine => [machine.id, machine]))
  const labelFor = (machineID: string) => {
    const machine = machinesByID.get(machineID)
    return machine ? machineLabel(machine) : machineID
  }
  const byName = (a: DispatchInstanceChoice, b: DispatchInstanceChoice) =>
    a.instance.name.localeCompare(b.instance.name, undefined, { sensitivity: 'base' }) || a.instance.id.localeCompare(b.instance.id)

  const listed: DispatchInstanceChoice[] = []
  const groups = new Map<string, DispatchInstanceChoice[]>()
  for (const instance of instances) {
    if (!instance.enabled) continue
    const choice: DispatchInstanceChoice = {
      instance,
      machineLabel: labelFor(instance.machine_id),
      listedBy: listedBy.get(instance.id) ?? [],
    }
    if (choice.listedBy.length > 0) {
      listed.push(choice)
      continue
    }
    const group = groups.get(instance.machine_id) ?? []
    group.push(choice)
    groups.set(instance.machine_id, group)
  }
  listed.sort(byName)
  const byMachine = [...groups.entries()]
    .map(([machineID, choices]) => ({ machineID, machineLabel: labelFor(machineID), choices: choices.sort(byName) }))
    .sort((a, b) => a.machineLabel.localeCompare(b.machineLabel, undefined, { sensitivity: 'base' }))
  return { listed, byMachine }
}

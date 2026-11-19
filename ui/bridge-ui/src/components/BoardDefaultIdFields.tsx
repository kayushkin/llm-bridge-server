import { useMemo } from 'react'
import { useBridgeConfig } from '../context'
import { bundleLabel, type BoardDefaultField } from '../kanbanBoardSettings'
import { pickablePrincipals, usePrincipals } from '../usePrincipals'
import { useAgentCatalog } from '../useResourceCatalogs'
import { useBridgeInstances } from '../useBridgeInstances'
import { useBridgeMachines } from '../useBridgeMachines'
import { useBundles } from '../useBundles'
import { machineLabel } from '../grantResources'

// The id pickers for the four defaults a board — and each of its tag rules —
// can set: principal, agent, instance, bundle. Shared by the settings page's
// Defaults and Tag rules sections so both offer exactly the same choices from
// the same owners.

export interface PickerOption {
  value: string
  label: string
}

/** What one owner offers for one default. */
export interface DefaultPicker {
  /** Null while the owner has not answered. */
  options: PickerOption[] | null
  optionsError: string | null
  /** Set when there is no owner to ask on this host: a text input is drawn
   *  and this says why. */
  unavailable: string | null
}

export type DefaultPickers = Record<BoardDefaultField, DefaultPicker>

/** Short names for the four defaults, for a rule row or a preview table. */
export const DEFAULT_FIELD_LABELS: Record<BoardDefaultField, string> = {
  default_principal_id: 'Principal',
  default_agent_id: 'Agent',
  default_instance_id: 'Instance',
  default_bundle_id: 'Bundle',
}

/**
 * The four owners' offers, read once for the page: principal-store's enabled
 * principals, agent-store's enabled agents (numeric ids), the bridge's enabled
 * instances, bundle-store's enabled bundles (numeric ids).
 */
export function useDefaultPickers(): DefaultPickers {
  const { bundleStoreBasePath } = useBridgeConfig()

  const principals = usePrincipals()
  const principalOptions = useMemo<PickerOption[] | null>(() => {
    if (!principals.enabled) return null
    if (principals.loading && principals.list.length === 0) return null
    return pickablePrincipals(principals.list, { query: '', kind: 'all', excludeIDs: [] })
      .map(principal => ({ value: principal.id, label: `${principal.display_name} (${principal.kind}, ${principal.id})` }))
  }, [principals])

  const agents = useAgentCatalog('')
  const agentOptions = useMemo<PickerOption[] | null>(
    () => agents.matches
      ? agents.matches.filter(option => !option.disabled).map(option => ({ value: option.id, label: `${option.label} (${option.detail}, id ${option.id})` }))
      : null,
    [agents.matches],
  )

  const { instances, loading: instancesLoading, error: instancesError } = useBridgeInstances()
  const { machines } = useBridgeMachines()
  const instanceOptions = useMemo<PickerOption[] | null>(() => {
    if (instancesLoading && instances.length === 0) return null
    return instances.filter(instance => instance.enabled).map(instance => {
      const machine = machines.find(candidate => candidate.id === instance.machine_id)
      return { value: instance.id, label: `${instance.name} — ${instance.harness_type} · ${machine ? machineLabel(machine) : instance.machine_id}` }
    })
  }, [instances, instancesLoading, machines])

  const bundles = useBundles()
  const bundleOptions = useMemo<PickerOption[] | null>(
    () => bundles.bundles
      ? bundles.bundles.filter(bundle => bundle.enabled).map(bundle => ({ value: String(bundle.id), label: `${bundleLabel(bundle)} (id ${bundle.id})` }))
      : null,
    [bundles.bundles],
  )

  return {
    default_principal_id: {
      options: principalOptions,
      optionsError: principals.error,
      unavailable: principals.enabled ? null : 'This host has no route to principal-store, so the id is typed here.',
    },
    default_agent_id: { options: agentOptions, optionsError: agents.error, unavailable: agents.unavailable },
    default_instance_id: { options: instanceOptions, optionsError: instancesError, unavailable: null },
    default_bundle_id: {
      options: bundleOptions,
      optionsError: bundles.error,
      unavailable: bundleStoreBasePath ? null : 'This host has no route to bundle-store, so the id is typed here.',
    },
  }
}

/** A stored id as its owner names it, or the raw id when the owner does not
 *  offer it (disabled, gone, or not loaded) — never nothing. */
export function pickerValueLabel(picker: DefaultPicker, value: string): string {
  return picker.options?.find(option => option.value === value)?.label ?? value
}

/**
 * One id-valued setting: a select over what the owner offers, or a plain text
 * input when this host has no route to the owner. The stored value is always
 * shown, even when the owner no longer offers it — the record is true whatever
 * the list says — and "clear" puts '' on the form, which is the wire's own word
 * for unsetting it.
 */
export function IdField({ id, label, help, value, onChange, options, optionsError, unavailable }: {
  id: string
  label: string
  /** Omitted where the section's own help already says it. */
  help?: string
  value: string
  onChange: (next: string) => void
} & DefaultPicker) {
  const offered = options ?? []
  const shownOptions = value && !offered.some(option => option.value === value)
    ? [...offered, { value, label: `${value} (not offered here)` }]
    : offered
  const helpText = [help, unavailable].filter(Boolean).join(' ')
  return (
    <div className="bks-field" data-field={id}>
      <label className="bks-field-label" htmlFor={id}>{label}</label>
      <div className="bks-id-row">
        {unavailable ? (
          <input id={id} className="bks-id-input" value={value} onChange={e => onChange(e.target.value)} />
        ) : (
          <select id={id} className="bks-id-select" value={value} onChange={e => onChange(e.target.value)} disabled={options === null && !value}>
            <option value="">{options === null ? 'loading…' : '— none —'}</option>
            {shownOptions.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
        )}
        {value && (
          <button type="button" className="bp-cancel" onClick={() => onChange('')} title={`Unset the ${label.toLowerCase()} on save`}>clear</button>
        )}
      </div>
      {helpText && <p className="bks-help">{helpText}</p>}
      {optionsError && <div className="bridge-error bks-error">{optionsError}</div>}
    </div>
  )
}

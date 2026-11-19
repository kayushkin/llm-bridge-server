import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useBridgeConfig } from '../context'
import { getEffectiveDefaultsForTags, getTagRules, putTagRules } from '../kanbanStoreClient'
import { BOARD_DEFAULT_FIELDS, defaultSourceLabel } from '../kanbanBoardSettings'
import {
  TAG_RULES_PRECEDENCE_HELP_TEXT, emptyTagRuleDraft, moveTagRuleDown, moveTagRuleUp, parseRuleTags, removeTagRule,
  tagRulesDirty, tagRulesDraftOf, tagRulesDraftToWire, type TagRuleDraft,
} from '../kanbanTagRules'
import { DEFAULT_FIELD_LABELS, IdField, pickerValueLabel, useDefaultPickers, type DefaultPickers } from './BoardDefaultIdFields'
import { SettingsSection } from './settings/SettingsSection'
import type { Board, BoardTagRule, EffectiveDefaults } from '@kayushkin/kanban-store-types'

/** How long the preview waits after the last keystroke before asking the store. */
const PREVIEW_DELAY_MS = 250

/**
 * Tag rules: the board's defaults overridden for cards carrying particular
 * tags. An ordered list edited as a whole and saved with one PUT that keeps
 * each stored rule's id; a refusal is shown in kanban-store's words (it names
 * `rules[i]` and its tags) and nothing is written; after a save the list is
 * re-read from the store and the form redrawn from it.
 *
 * The preview below asks the store what a card with some tags would get. It
 * never resolves rules on the client — the precedence lives in kanban-store.
 */
export function BoardTagRulesSection({ board }: { board: Board }) {
  const { fetch: fetchFn, kanbanStoreBasePath: base } = useBridgeConfig()
  const pickers = useDefaultPickers()
  // Null until the first read answers. Save stays disabled while it is null:
  // a PUT replaces the whole list, so saving over rules never read would wipe them.
  const [stored, setStored] = useState<BoardTagRule[] | null>(null)
  const [drafts, setDrafts] = useState<TagRuleDraft[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)
  // Bumped on every successful save so the preview re-asks with the new rules.
  const [savedRulesVersion, setSavedRulesVersion] = useState(0)
  const nextNewRuleNumber = useRef(1)
  const readTicket = useRef(0)

  const reload = useCallback(async (): Promise<boolean> => {
    const mine = ++readTicket.current
    const result = await getTagRules(fetchFn, base, board.id)
    if (mine !== readTicket.current) return false
    if (!result.ok) {
      setLoadError(result.error)
      return false
    }
    setLoadError(null)
    setStored(result.value.rules)
    setDrafts(tagRulesDraftOf(result.value.rules))
    return true
  }, [fetchFn, base, board.id])

  useEffect(() => { void reload() }, [reload])

  const updateDraft = (draftKey: string, change: (draft: TagRuleDraft) => TagRuleDraft) => {
    setSaved(false)
    setDrafts(previous => previous.map(draft => (draft.draftKey === draftKey ? change(draft) : draft)))
  }
  const reshape = (change: (previous: TagRuleDraft[]) => TagRuleDraft[]) => {
    setSaved(false)
    setDrafts(change)
  }

  const dirty = stored !== null && tagRulesDirty(stored, drafts)

  const save = async () => {
    setSaving(true)
    setSaved(false)
    const result = await putTagRules(fetchFn, base, board.id, tagRulesDraftToWire(drafts))
    if (!result.ok) {
      setSaving(false)
      setSaveError(result.error)
      return
    }
    setSaveError(null)
    const reread = await reload()
    setSaving(false)
    setSaved(reread)
    setSavedRulesVersion(version => version + 1)
  }

  return (
    <SettingsSection id="tag-rules" title="Tag rules" scope="board" storedBy="kanban-store · board tag rules"
      save={{ dirty, state: { saving, error: saveError, saved }, onSave: () => { void save() }, label: 'Save tag rules', extra: (
        <>
        <button
          type="button"
          className="bi-add-btn"
          disabled={stored === null}
          onClick={() => reshape(previous => [...previous, emptyTagRuleDraft(`new-rule-${nextNewRuleNumber.current++}`)])}
        >+ Add rule</button>
        </>
      ) }}
      after={<TagRulesPreview board={board} storedRules={stored} savedRulesVersion={savedRulesVersion} pickers={pickers} />}>
      <p className="bks-help">
        Override this board’s defaults for cards carrying particular tags — the card’s noteboard tags, matched exactly.
      </p>
      <p className="bks-help bks-precedence">{TAG_RULES_PRECEDENCE_HELP_TEXT}</p>
      {loadError && <div className="bridge-error bks-error">{loadError}</div>}

      {stored === null ? (
        !loadError && <div className="bi-loading">Loading…</div>
      ) : (
        <div className="bks-tag-rules">
          {drafts.length === 0 && <span className="bks-help">No tag rules: every card on this board gets the board’s defaults.</span>}
          {drafts.map((draft, index) => (
            <TagRuleRow
              key={draft.draftKey}
              draft={draft}
              index={index}
              count={drafts.length}
              pickers={pickers}
              onChange={change => updateDraft(draft.draftKey, change)}
              onMoveUp={() => reshape(previous => moveTagRuleUp(previous, index))}
              onMoveDown={() => reshape(previous => moveTagRuleDown(previous, index))}
              onRemove={() => reshape(previous => removeTagRule(previous, index))}
            />
          ))}
        </div>
      )}

    </SettingsSection>
  )
}

function TagRuleRow({ draft, index, count, pickers, onChange, onMoveUp, onMoveDown, onRemove }: {
  draft: TagRuleDraft
  index: number
  count: number
  pickers: DefaultPickers
  onChange: (change: (draft: TagRuleDraft) => TagRuleDraft) => void
  onMoveUp: () => void
  onMoveDown: () => void
  onRemove: () => void
}) {
  const tags = parseRuleTags(draft.tagsText)
  const tagsInputID = `bks-tag-rule-${draft.draftKey}-tags`
  return (
    <div className="bks-tag-rule" data-tag-rule-id={draft.id ?? undefined} data-tag-rule-index={index}>
      <div className="bks-tag-rule-head">
        <span className="bks-tag-rule-position" title="Checked in this order, top first">{index + 1}</span>
        <div className="bks-field bks-tag-rule-tags">
          <label className="bks-field-label" htmlFor={tagsInputID}>Tags — a card must carry all of them</label>
          <input
            id={tagsInputID}
            value={draft.tagsText}
            placeholder="cat:product, urgency:high"
            onChange={e => { const tagsText = e.target.value; onChange(previous => ({ ...previous, tagsText })) }}
          />
        </div>
        <div className="bks-tag-rule-buttons">
          <button type="button" className="bp-cancel" disabled={index === 0} title="Check this rule one place earlier" onClick={onMoveUp}>↑ up</button>
          <button type="button" className="bp-cancel" disabled={index === count - 1} title="Check this rule one place later" onClick={onMoveDown}>↓ down</button>
          <button type="button" className="bp-cancel" title="Remove this rule on save" onClick={onRemove}>remove</button>
        </div>
      </div>
      <div className="bks-tag-chips">
        {tags.length === 0
          ? <span className="bks-help">no tags</span>
          : tags.map((tag, at) => <span key={`${at}:${tag}`} className="bks-tag-chip">{tag}</span>)}
      </div>
      <div className="bks-tag-rule-defaults">
        {BOARD_DEFAULT_FIELDS.map(field => (
          <IdField
            key={field}
            id={`bks-tag-rule-${draft.draftKey}-${field}`}
            label={DEFAULT_FIELD_LABELS[field]}
            value={draft.defaults[field]}
            onChange={next => onChange(previous => ({ ...previous, defaults: { ...previous.defaults, [field]: next } }))}
            {...pickers[field]}
          />
        ))}
      </div>
    </div>
  )
}

/**
 * Type some tags, see what a card carrying exactly those would get on this
 * board and where each value comes from — kanban-store's own resolution over
 * the SAVED rules, never a client copy of it.
 */
function TagRulesPreview({ board, storedRules, savedRulesVersion, pickers }: {
  board: Board
  storedRules: BoardTagRule[] | null
  savedRulesVersion: number
  pickers: DefaultPickers
}) {
  const { fetch: fetchFn, kanbanStoreBasePath: base } = useBridgeConfig()
  const [tagsText, setTagsText] = useState('')
  const tagsKey = useMemo(() => JSON.stringify(parseRuleTags(tagsText)), [tagsText])
  // The board's own defaults are part of the answer, so a Defaults save
  // (which moves `updated_at`) re-asks too.
  const readKey = `${tagsKey} ${savedRulesVersion} ${board.updated_at}`
  const [answer, setAnswer] = useState<{ forReadKey: string; effective: EffectiveDefaults | null; error: string | null } | null>(null)

  useEffect(() => {
    let canceled = false
    const timer = setTimeout(() => {
      getEffectiveDefaultsForTags(fetchFn, base, board.id, JSON.parse(tagsKey) as string[]).then(result => {
        if (canceled) return
        setAnswer(result.ok
          ? { forReadKey: readKey, effective: result.value, error: null }
          : { forReadKey: readKey, effective: null, error: result.error })
      })
    }, PREVIEW_DELAY_MS)
    return () => { canceled = true; clearTimeout(timer) }
  }, [fetchFn, base, board.id, tagsKey, readKey])

  const current = answer?.forReadKey === readKey ? answer : null
  const ruleWording = (ruleID: string) => {
    const rule = storedRules?.find(candidate => candidate.id === ruleID)
    return rule ? `rule ${rule.position + 1} (${rule.tags.join(' + ')})` : `rule ${ruleID}`
  }

  return (
    <div className="bks-preview" data-part="tag-rules-preview">
      <div className="bks-field">
        <label className="bks-field-label" htmlFor={`bks-tag-rules-preview-${board.id}`}>Preview — a card tagged</label>
        <input
          id={`bks-tag-rules-preview-${board.id}`}
          value={tagsText}
          placeholder="cat:product urgency:high"
          onChange={e => setTagsText(e.target.value)}
        />
        <span className="bks-help">
          Asks kanban-store what a card carrying exactly these tags gets on this board, from the saved rules — save first to preview an edit.
        </span>
      </div>
      {!current ? (
        <div className="bi-loading">Asking kanban-store…</div>
      ) : current.error ? (
        <div className="bridge-error bks-error">{current.error}</div>
      ) : current.effective && (
        <>
          <div className="bks-help" data-matched-rule-ids={current.effective.matched_rule_ids.join(' ')}>
            {current.effective.matched_rule_ids.length === 0
              ? 'Matches no rule.'
              : `Matches ${current.effective.matched_rule_ids.map(ruleWording).join(', ')}.`}
          </div>
          <table className="bks-preview-table">
            <tbody>
              {BOARD_DEFAULT_FIELDS.map(field => {
                const resolved = current.effective!.defaults[field]
                return (
                  <tr key={field} data-field={field} data-source-kind={resolved?.source.kind}>
                    <th>{DEFAULT_FIELD_LABELS[field]}</th>
                    <td>{resolved ? pickerValueLabel(pickers[field], resolved.value) : <span className="bks-help">none — nothing sets it</span>}</td>
                    <td className="bks-default-source">{resolved ? defaultSourceLabel(resolved.source) : ''}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}

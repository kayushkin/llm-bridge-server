import { ApiError } from './ApiClient.js';

// Noteboard is a SECOND backend, not part of the bridge gateway: it answers on
// its own base path (dash proxies it at `/api/noteboard/*`, llm-bridge at
// `/api/bridge/*`). It therefore gets its own client rather than a method on
// ApiClient, whose `basePath` is the bridge root and would have to be bypassed
// per call to reach here.
//
// Ref chips are the only reason chat-core talks to noteboard at all, and they
// read one item at a time by id. This client stays that narrow on purpose: it
// is not a general noteboard SDK, and a caller that needs list/search/mutation
// should add methods deliberately rather than find them already here.

/** The item types noteboard stores (`model.ValidType`). `rank` is a ranked
 *  list and `workspace` is an agent's durable working memory. */
export type NoteboardItemType = 'note' | 'todo' | 'rank' | 'workspace';

/**
 * One noteboard item, mapped key-for-key from `model.Item` (noteboard
 * `model/model.go`). Field names are kept in the wire's snake_case rather than
 * camelCased, because this is a foreign store's record passing through
 * unchanged — renaming its fields here would make the chip's rows and the
 * noteboard API read as two different records.
 *
 * Optional fields are optional on the wire too (Go `omitempty`), and an absent
 * one is left absent rather than defaulted: `due_at` missing means "no due
 * date", which is not the same as any particular date, and `held_at` missing is
 * what distinguishes a live item from a parked one.
 */
export interface NoteboardItem {
  id: string;
  type: NoteboardItemType | string;
  title: string;
  body: string;
  tags: string[];
  priority: number;
  rank: number;
  status: string;
  list_id: string;
  links: string[];
  created_by: string;
  created_at: string;
  updated_at: string;
  due_at?: string;
  parent_id?: string;
  /** Recurrence RULE, not a due date — `due_at` stays the single source of
   *  truth for when the item is next due. Carried verbatim; chat-core does not
   *  expand an RRULE (noteboard's `/occurrences` does that). */
  schedule?: NoteboardSchedule;
  /** Reversible delete. Deliberately not the `archived` status: archived is a
   *  state the user chose, deleted is the item being taken away. */
  deleted_at?: string;
  /** The agent gate — work the user has parked. An item can be open AND held,
   *  so this is not a status either. */
  held_at?: string;
  hold_reason?: string;
  auto_hold_at_usd?: number;
}

/** The recurrence rule attached to an item. Passed through as noteboard sends
 *  it; only the fields the chip displays are named. */
export interface NoteboardSchedule {
  dtstart?: string;
  tzid?: string;
  rrule?: string;
  rdate?: string[];
  exdate?: string[];
  mode?: string;
  remind?: {
    lead?: string[];
    channels?: string[];
    nag?: string;
  };
}

/** The auth'd fetch + noteboard root. dash passes its cookie-credentialed
 *  `apiFetch` and `basePath = '/api/noteboard'`, which its Go proxy strips
 *  before forwarding, so the paths below are noteboard's own. */
export interface NoteboardClientConfig {
  fetch: typeof fetch;
  basePath: string;
}

/**
 * Thin read client for noteboard items. Like ApiClient it is an honest
 * pass-through — no formatting, no fallback — and throws loudly on a non-2xx so
 * a chip shows "couldn't load" rather than an empty panel that reads as an
 * empty item.
 */
export class NoteboardClient {
  private readonly doFetch: typeof fetch;
  readonly basePath: string;

  constructor(config: NoteboardClientConfig) {
    this.doFetch = config.fetch;
    this.basePath = config.basePath.replace(/\/$/, '');
  }

  /**
   * One item by id (`GET /api/items/{id}`).
   *
   * The response carries the item's own `type`, and THAT is the authoritative
   * answer to whether this id is a note, a todo, a rank list or a workspace.
   * The cue word a message author typed in front of the uuid ("todo: <id>") is
   * only what made the matcher look; it is not evidence about the row. A chip
   * must label itself from this field, never from the cue.
   *
   * A deleted item still resolves here — noteboard's delete is reversible and
   * `GET` by id returns the row with `deleted_at` set — so the panel can say
   * "deleted" instead of failing, which is the more useful answer for an id
   * quoted in an old message.
   */
  async getItem(id: string): Promise<NoteboardItem> {
    const path = `/api/items/${encodeURIComponent(id)}`;
    const res = await this.doFetch(`${this.basePath}${path}`);
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      throw new ApiError({
        message: `GET ${path} failed: ${res.status} ${res.statusText}`,
        status: res.status,
        body,
        method: 'GET',
        path,
      });
    }
    return (await res.json()) as NoteboardItem;
  }
}

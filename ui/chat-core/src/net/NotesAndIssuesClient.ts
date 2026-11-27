import { ApiError } from './ApiClient.js';

// Notes and issues are backends of their own, not part of the bridge gateway:
// each answers on its own base path (llm-bridge-server serves them at
// `/notes` and `/issues/items`). They were one store, noteboard, until
// 2026-11-27, when noteboard's todos became issues and its notes, ranked list
// entries and workspaces stayed in notes. The two still share one id space,
// so an id a message quotes names an item in one of them, and this client
// asks notes first and issues when notes has no such item.
//
// Ref chips are the only reason chat-core reads either, and they read one
// item at a time by id. This client stays that narrow on purpose: it is not a
// general SDK, and a caller that needs list/search/mutation should add methods
// deliberately rather than find them already here.

/** The item types (`model.ValidType`): `todo` is an issue, kept by issues;
 *  `note`, `rank` (a ranked list entry) and `workspace` (an agent's durable
 *  working memory) are kept by notes. */
export type NoteOrIssueType = 'note' | 'todo' | 'rank' | 'workspace';

/**
 * One note or issue, mapped key-for-key from `model.Item`
 * (`internal/tasktracking/items/model/model.go`). Field names are kept in the wire's snake_case rather than
 * camelCased, because this is a foreign store's record passing through
 * unchanged — renaming its fields here would make the chip's rows and the
 * items API read as two different records.
 *
 * Optional fields are optional on the wire too (Go `omitempty`), and an absent
 * one is left absent rather than defaulted: `due_at` missing means "no due
 * date", which is not the same as any particular date, and `held_at` missing is
 * what distinguishes a live item from a parked one.
 */
export interface NoteOrIssue {
  id: string;
  type: NoteOrIssueType | string;
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
   *  expand an RRULE (the items routes' `/occurrences` does that). */
  schedule?: ItemSchedule;
  /** Reversible delete. Deliberately not the `archived` status: archived is a
   *  state the user chose, deleted is the item being taken away. */
  deleted_at?: string;
  /** The agent gate — work the user has parked. An item can be open AND held,
   *  so this is not a status either. */
  held_at?: string;
  hold_reason?: string;
  auto_hold_at_usd?: number;
}

/** The recurrence rule attached to an item. Passed through as the store sends
 *  it; only the fields the chip displays are named. */
export interface ItemSchedule {
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

/** The auth'd fetch, and the roots of notes and of the content of issues.
 *  llm-bridge-server's own front end passes `/notes` and `/issues`; a host
 *  that proxies them passes its own prefixes, and the paths below are the
 *  stores' own. */
export interface NotesAndIssuesClientConfig {
  fetch: typeof fetch;
  notesBasePath: string;
  issuesBasePath: string;
}

/**
 * Thin read client for notes and issues. Like ApiClient it is an honest
 * pass-through — no formatting — and throws loudly on a non-2xx other than
 * the 404 that sends it to the other store, so a chip shows "couldn't load"
 * rather than an empty panel that reads as an empty item.
 */
export class NotesAndIssuesClient {
  private readonly doFetch: typeof fetch;
  readonly notesBasePath: string;
  readonly issuesBasePath: string;

  constructor(config: NotesAndIssuesClientConfig) {
    this.doFetch = config.fetch;
    this.notesBasePath = config.notesBasePath.replace(/\/$/, '');
    this.issuesBasePath = config.issuesBasePath.replace(/\/$/, '');
  }

  /**
   * One item by id: notes' `GET /items/{id}`, and when notes has none,
   * issues' `GET /items/{id}`. The two hold disjoint ids, so this is a
   * lookup in the one store that has the item, not a guess.
   *
   * The response carries the item's own `type`, and THAT is the authoritative
   * answer to whether this id is a note, a todo, a rank list or a workspace.
   * The cue word a message author typed in front of the uuid ("todo: <id>") is
   * only what made the matcher look; it is not evidence about the row. A chip
   * must label itself from this field, never from the cue.
   */
  async getItem(id: string): Promise<NoteOrIssue> {
    const itemPath = `/items/${encodeURIComponent(id)}`;
    const notes = await this.doFetch(`${this.notesBasePath}${itemPath}`);
    if (notes.ok) return (await notes.json()) as NoteOrIssue;
    if (notes.status !== 404) throw await failed(notes, `${this.notesBasePath}${itemPath}`);
    const issues = await this.doFetch(`${this.issuesBasePath}${itemPath}`);
    if (issues.ok) return (await issues.json()) as NoteOrIssue;
    throw await failed(issues, `${this.issuesBasePath}${itemPath}`);
  }
}

async function failed(res: Response, path: string): Promise<ApiError> {
  const body = await res.text().catch(() => '');
  return new ApiError({
    message: `GET ${path} failed: ${res.status} ${res.statusText}`,
    status: res.status,
    body,
    method: 'GET',
    path,
  });
}

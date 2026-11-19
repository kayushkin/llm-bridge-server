import type { FetchFn } from './types'

// mailstack, as the host proxies it (`mailBasePath`; dash's `/api/mail` maps
// onto mailstack's `/api`). The card drawer reads one message from it; the
// board settings page reads the account list so a classifier's
// `mail_account_ids` can be ticked rather than typed.

/** One account as mailstack's `GET /api/accounts` lists it — the fields the
 *  settings page shows. `id` is what a board's classifier stores. */
export interface MailAccount {
  id: string
  label: string
  provider: string
  enabled: boolean
}

export type MailstackResult<T> = { ok: true; value: T } | { ok: false; error: string }

/** `GET {mailBasePath}/accounts` → `{"accounts":[…]}`. */
export async function listMailAccounts(fetchFn: FetchFn, mailBasePath: string): Promise<MailstackResult<MailAccount[]>> {
  let res: Response
  try {
    res = await fetchFn(`${mailBasePath}/accounts`)
  } catch (err) {
    return { ok: false, error: `list mail accounts: ${err instanceof Error ? err.message : String(err)}` }
  }
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    return { ok: false, error: `list mail accounts: ${text || `HTTP ${res.status}`}` }
  }
  const body = (await res.json()) as { accounts?: unknown }
  if (!Array.isArray(body?.accounts)) return { ok: false, error: 'list mail accounts: the answer carried no accounts list' }
  return { ok: true, value: body.accounts as MailAccount[] }
}

import type { SessionConfig } from '../net/types.js';
import type { NewSessionOpts } from './ChatStore.js';

/** The `POST /sessions/{id}/config` body a freshly-created session owes its pending
 *  pane, or `null` when the pane carried no settings and the call should be skipped
 *  entirely.
 *
 *  `POST /sessions` deliberately takes the target only (instance + harness), so every
 *  runtime setting arrives on a second call. This function is the whole decision about
 *  what goes in that call, extracted from the send path so it can be tested without a
 *  browser — the two values that make it subtle are exactly the two a UI test is least
 *  likely to produce:
 *
 *    - `maxBudget: 0` means NO CEILING on the server, not "halt immediately". A
 *      truthiness check reads it as absent and the session silently comes up with
 *      whatever ceiling the server defaults to, which is the opposite instruction.
 *    - `disabledTools: []` means "disable nothing", which is a decision. Absent means
 *      "inherit", which is not.
 *
 *  Absent fields are omitted from the body rather than sent as null: `ApiClient.setConfig`
 *  maps present keys only, and a null would ask the server to clear a setting nobody
 *  asked to clear. */
export function pendingSessionConfig(opts: NewSessionOpts | null | undefined): SessionConfig | null {
  if (!opts) return null;
  const config: SessionConfig = {};
  if (opts.model) config.model = opts.model;
  if (opts.effort) config.effort = opts.effort;
  if (opts.maxBudget !== undefined) config.maxBudget = opts.maxBudget;
  if (opts.disabledTools !== undefined) config.disabledTools = opts.disabledTools;
  return Object.keys(config).length > 0 ? config : null;
}

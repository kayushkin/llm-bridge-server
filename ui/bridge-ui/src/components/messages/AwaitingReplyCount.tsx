import { useAwaitingReply } from '../../useAwaitingReply'

/** How many conversations are waiting on a reply, as a small count beside a
 *  tab or a heading; nothing when none are. */
export function AwaitingReplyCount() {
  const { overview } = useAwaitingReply()
  const count = overview?.waiting.length ?? 0
  if (count === 0) return null
  return <span className="bc-awaiting-count" aria-label={`${count} waiting`}>{count}</span>
}

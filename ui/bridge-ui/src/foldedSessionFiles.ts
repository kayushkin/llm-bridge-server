// Which shared pictures, videos and groups of pictures a person folded away in the
// chat, kept in this browser so a fold survives a reload and a scroll (the transcript
// is a virtual list, so a row's own state is lost each time it leaves the screen).
// The pure rules are tested in test/foldedSessionFiles.test.ts; the hook is
// `useFoldedSessionFile` in components/chat/SessionFileChip.tsx.

/** The most folds kept. Past it the oldest are forgotten, and those files draw open
 *  again; a list that only grew would fill the browser's storage over the years. */
export const FOLDED_SESSION_FILES_LIMIT = 2000

/** The stored list, oldest fold first. Anything that is not a list of strings — an
 *  empty store, a row another version wrote — reads as no folds. */
export function parseFoldedSessionFiles(stored: string | null): string[] {
  if (!stored) return []
  let parsed: unknown
  try {
    parsed = JSON.parse(stored)
  } catch {
    return []
  }
  if (!Array.isArray(parsed)) return []
  return parsed.filter((key): key is string => typeof key === 'string' && key !== '')
}

/** The list after one key is folded or unfolded. A new fold goes last; when the list
 *  is then over the limit, the oldest folds are dropped. */
export function withSessionFileFold(
  folded: readonly string[],
  key: string,
  fold: boolean,
  limit: number = FOLDED_SESSION_FILES_LIMIT,
): string[] {
  const others = folded.filter((candidate) => candidate !== key)
  if (!fold) return others
  const next = [...others, key]
  return next.length > limit ? next.slice(next.length - limit) : next
}

/** The fold key of one shared file: its file-store id. */
export function sessionFileFoldKey(fileId: string): string {
  return `file:${fileId}`
}

/** The fold key of a group of pictures shared together: the id of its first file,
 *  which stays the same as later pictures join the group. */
export function imageGroupFoldKey(firstFileId: string): string {
  return `group:${firstFileId}`
}

// Which shared image a file mention in prose names, as a pure rule so it is tested
// (test/sessionImageMentions.test.ts). The chip is ImageRefChip in
// components/chat/FileRefChip.tsx.

/** What the rule reads of a shared image. */
export interface MentionableImage {
  key: string
  /** The file's name as it was shared, without a directory. */
  name: string
  /** RFC 3339, when it was shared; absent for an image that was not shared as a file. */
  sharedAt?: string
}

/** The last segment of a path: what an agent's `/tmp/shots/home.png` and a shared
 *  file called `home.png` have in common. */
export function baseNameOf(path: string): string {
  return path.slice(path.lastIndexOf('/') + 1)
}

/**
 * The shared image a mention names, or null. A mention names an image when its last
 * path segment is the image's file name. When several were shared under one name —
 * the same screenshot taken again later — it is the newest shared no later than the
 * message, since that is the one the writer could have meant; a message with no time,
 * or written before all of them, gets the newest.
 */
export function imageNamedByMention<T extends MentionableImage>(
  images: readonly T[],
  mentionPath: string,
  writtenAt: string | null,
): T | null {
  const name = baseNameOf(mentionPath)
  if (!name) return null
  const matches = images.filter((image) => image.sharedAt !== undefined && image.name === name)
  if (matches.length === 0) return null
  const newestFirst = [...matches].sort((a, b) => Date.parse(b.sharedAt!) - Date.parse(a.sharedAt!))
  if (writtenAt) {
    const written = Date.parse(writtenAt)
    const before = newestFirst.find((image) => Date.parse(image.sharedAt!) <= written)
    if (before) return before
  }
  return newestFirst[0]!
}

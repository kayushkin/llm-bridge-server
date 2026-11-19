/**
 * Turning an entity ref into something you can click.
 *
 * kanban-store deliberately does not resolve refs — it publishes an entity-type
 * registry and leaves resolution to the client, so that adding a type does not
 * mean teaching the store about another service. That leaves this: the one place
 * that knows a `pull_request` ref is a GitHub pull request and what its URL is.
 *
 * Every function here refuses to guess. A ref that does not match the shape its
 * type promises returns null and renders as plain text, because a link that
 * 404s is worse than a string you can copy: it looks like the record is wrong
 * when it is the URL that is.
 */

/** Where a ref points, and what to call it on screen. */
export interface EntityTarget {
  href: string
  /** Short human label — the part worth reading, not the whole ref. */
  label: string
  /** Whether this leaves the app. Used to set rel/target. */
  external: boolean
}

const GITHUB = 'https://github.com'

/**
 * `owner/name#123` — the canonical identity of a pull request.
 *
 * Repo-qualified because a bare number names a different pull request in every
 * repository that has one.
 */
const PULL_REQUEST = /^([\w.-]+)\/([\w.-]+)#(\d+)$/

/**
 * `owner/name@<sha>` — a commit, with the sha written in full.
 *
 * Full and not abbreviated: an abbreviation is a prefix that stays unique right
 * up until it does not. Seven characters is accepted on the way in because
 * plenty of tools emit it, but nothing here shortens one that arrived complete.
 */
const COMMIT = /^([\w.-]+)\/([\w.-]+)@([0-9a-f]{7,40})$/i

/** A remote URL we are willing to send a browser to. */
const HTTP_URL = /^https?:\/\/[^\s]+$/

export function entityTarget(entityType: string, entityRef: string): EntityTarget | null {
  const ref = (entityRef ?? '').trim()
  if (!ref) return null

  switch (entityType) {
    case 'pull_request': {
      const m = PULL_REQUEST.exec(ref)
      if (!m) return null
      const [, owner, name, number] = m
      return { href: `${GITHUB}/${owner}/${name}/pull/${number}`, label: `#${number}`, external: true }
    }
    case 'commit': {
      const m = COMMIT.exec(ref)
      if (!m) return null
      const [, owner, name, sha] = m
      // The sha is shortened for READING only. The href carries it in full, so
      // the link cannot become ambiguous as the repository grows.
      return { href: `${GITHUB}/${owner}/${name}/commit/${sha}`, label: sha.slice(0, 8), external: true }
    }
    case 'git_repo': {
      if (!HTTP_URL.test(ref)) return null
      return { href: ref, label: ref.replace(/^https?:\/\/(www\.)?github\.com\//, ''), external: true }
    }
    default:
      return null
  }
}

/**
 * A ref that is a path on this machine rather than something to open.
 *
 * `repo` is registered as a local filesystem path and has no upstream to
 * resolve against, so it is shown as one — monospaced, not underlined, not
 * pretending to be a link.
 */
export function isLocalPathRef(entityType: string): boolean {
  return entityType === 'repo'
}

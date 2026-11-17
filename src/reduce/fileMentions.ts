// A file an agent names in its prose, and which of its session's files it means.
//
// An agent writes `internal/server/renamer.go` or `sessions.go:707` in a code
// span, relative to wherever it was looking. The server lists every file the
// session's tools named (`GET /sessions/{id}/files-named-by-tools`, absolute
// paths that exist now); `filesMatchingMention` picks the ones a mention can
// mean. The text alone never produces a path: a mention that matches nothing on
// the list stays plain text.
//
// Only a whole single-backtick span is a mention (see remarkRefChips). File
// names in running prose are too often ordinary words with a dot.

/** One file on the session's list, as llm-bridge-server returns it. */
export interface FileNamedByTools {
  path: string;
  size: number;
  /** RFC 3339, when the file last changed on disk. */
  modified_at: string;
  /** What the file's first bytes look like (Go's http.DetectContentType). */
  media_type: string;
  /** How the server serves it: `text` from the content route, `image` from the
   *  image route, '' not at all (another kind, or too large). The chat links a
   *  mention only to a file served one way or the other. */
  served_as: 'text' | 'image' | '';
}

/** A file on the list with its current text. */
export interface FileNamedByToolsContent extends FileNamedByTools {
  content: string;
}

/** A mention split into the path and the line it points at, if any. */
export interface FileMention {
  path: string;
  /** First line named (`:707`, `:12-40`), 1-based. */
  line: number | null;
}

// Optional `~/`, `./` or `/`; any directories; a last segment with an extension
// that starts with a lowercase letter (so `config.Load` and `v1.2` are not
// files); an optional `:line`, `:line-line` or `:line:column`. The server's
// bareFileNamePattern uses the same extension rule.
const FILE_MENTION_PATTERN =
  /^(?:~\/|\.\/|\/)?(?:[\w.@+-]+\/)*[\w@+-][\w.@+-]*\.[a-z][A-Za-z0-9]{0,9}(?::(\d+)(?:[-:]\d+)?)?$/;
const LINE_SUFFIX = /:\d+(?:[-:]\d+)?$/;

/** The mention in a code span's text, or null when the span is not a file. */
export function fileMentionOf(codeSpanText: string): FileMention | null {
  const text = codeSpanText.trim();
  const match = FILE_MENTION_PATTERN.exec(text);
  if (!match) return null;
  return {
    path: text.replace(LINE_SUFFIX, ''),
    line: match[1] === undefined ? null : Number(match[1]),
  };
}

/**
 * The files on the list a mention can mean: the same absolute path, or every
 * file whose path ends in the mention at a directory boundary (`sessions.go`
 * matches `/r/internal/server/sessions.go`, not `/r/usersessions.go`). More than
 * one is common — the same relative path in a main clone and its worktree — and
 * the caller shows them all rather than guessing.
 */
export function filesMatchingMention(
  files: readonly FileNamedByTools[],
  mentionPath: string,
): FileNamedByTools[] {
  if (mentionPath.startsWith('/')) return files.filter((file) => file.path === mentionPath);
  // `~/x` cannot be expanded here; `/x` at the end of the path is what it names.
  const tail = mentionPath.startsWith('~/')
    ? mentionPath.slice(1)
    : `/${mentionPath.replace(/^\.\//, '')}`;
  return files.filter((file) => file.path.endsWith(tail));
}

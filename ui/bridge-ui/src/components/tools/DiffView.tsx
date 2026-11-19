import { structuredPatch, type Hunk } from 'diff'

// DiffView renders a single file's before/after as a unified-diff hunk list.
// Empty when the two sides match. Used by both EditRenderer (single file per
// tool call) and BashRenderer (one DiffView per file the bash command touched).
export function DiffView({ filePath, before, after }: { filePath: string; before: string; after: string }) {
  if (before === after) {
    return <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>No changes</div>
  }
  const patch = structuredPatch(filePath || 'file', filePath || 'file', before, after, '', '', { context: 3 })
  if (patch.hunks.length === 0) {
    return <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>No changes</div>
  }
  return (
    <pre className="bc-diff">
      {patch.hunks.map((h, i) => <HunkView key={i} hunk={h} />)}
    </pre>
  )
}

function HunkView({ hunk }: { hunk: Hunk }) {
  return (
    <div className="bc-diff-hunk">
      <div className="bc-diff-hunk-header">
        @@ -{hunk.oldStart},{hunk.oldLines} +{hunk.newStart},{hunk.newLines} @@
      </div>
      {hunk.lines.map((line, i) => {
        const cls = line.startsWith('+')
          ? 'bc-diff-add'
          : line.startsWith('-')
            ? 'bc-diff-del'
            : 'bc-diff-ctx'
        return <div key={i} className={cls}>{line}</div>
      })}
    </div>
  )
}

/**
 * A unified diff that ALREADY EXISTS, colored.
 *
 * `DiffView` above cannot do this job and the difference is the whole reason this
 * exists. It takes a file's `before` and `after` CONTENTS and computes the patch
 * itself with `structuredPatch`. The git endpoint
 * (`GET /sessions/{id}/git?repo=…`) does not return two versions of a file — it
 * returns `diff_unstaged` and `diff_staged`, which are the output of `git diff`,
 * already unified and possibly spanning many files. There is nothing to diff.
 *
 * So this renders what it is given, line by line, using the same `bc-diff-*`
 * classes and therefore the same colors as the tool cards. What it adds over the
 * `<pre>` the Git pane used to draw is only that: `+` green, `-` red, `@@` and the
 * `diff --git` / `index` / `+++` / `---` file headers set apart from the body.
 *
 * ⚠️ Order matters in the classifier below. A unified diff's `+++ b/path` and
 * `--- a/path` headers start with `+` and `-`, so testing for additions first
 * paints every file header as an added line — which is exactly wrong at the one
 * place a reader is trying to see where one file ends and the next begins.
 */
export function UnifiedDiffView({ diff }: { diff: string }) {
  if (!diff.trim()) {
    return <div className="bc-tool-output-code" style={{ opacity: 0.6 }}>No changes</div>
  }
  // `\n` split keeps a trailing empty line rather than dropping it; git's output ends
  // with a newline and swallowing it would silently shorten every diff by a row.
  const lines = diff.split('\n')
  return (
    <pre className="bc-diff">
      <div className="bc-diff-hunk">
        {lines.map((line, i) => (
          <div key={i} className={unifiedDiffLineClass(line)}>{line || ' '}</div>
        ))}
      </div>
    </pre>
  )
}

/** Which class a single line of a unified diff gets. Exported for the test that
 *  pins the header-before-sign ordering. */
export function unifiedDiffLineClass(line: string): string {
  // Headers FIRST — see the warning above.
  if (
    line.startsWith('diff --git') ||
    line.startsWith('index ') ||
    line.startsWith('+++ ') ||
    line.startsWith('--- ') ||
    line.startsWith('new file') ||
    line.startsWith('deleted file') ||
    line.startsWith('similarity index') ||
    line.startsWith('rename ')
  ) {
    return 'bc-diff-file-header'
  }
  if (line.startsWith('@@')) return 'bc-diff-hunk-header'
  if (line.startsWith('+')) return 'bc-diff-add'
  if (line.startsWith('-')) return 'bc-diff-del'
  return 'bc-diff-ctx'
}

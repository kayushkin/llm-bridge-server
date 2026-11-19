import { describe, expect, it } from 'vitest';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

// A raw NUL byte in a source file makes the whole FILE binary as far as every
// content search on the box is concerned, and the failure is silent in the
// direction of "nothing here".
//
// `src/reduce/otelDedup.ts` carried one from the day it was written (the port
// commit, 2026-07-27) until 2026-08-15: `contentKey` built its composite dedup
// key with a literal NUL separator instead of a backslash-u-0000 escape. The
// consequences, all measured on this host rather than reasoned about:
//
//   - `file(1)` reported the file as `data`, not as source.
//   - `grep -c import` on it returned NOTHING and exit 1, while GNU `grep -a`
//     found the same lines. The `grep` every agent here runs is a ugrep wrapper
//     invoked with `-I` (skip binary files), so the file answered every query
//     with zero matches and no warning.
//   - So every fleet-wide census run over this repo with a content search had
//     been quietly excluding it for three weeks. Its two failure-value producers
//     did not appear in the ranking that a sweep of exactly those producers was
//     working from.
//
// The escape and the raw byte are the identical string at runtime, so nothing is
// given up by requiring the escape. This case exists because the defect is
// invisible in every rendering of the file a reviewer normally looks at — an
// editor, a diff and `cat` all draw a NUL as nothing at all.
//
// Not scoped to a hand-listed set of files: the point is that a NUL can be
// introduced anywhere, and a list of the places it has already been is exactly
// the list of places it is now fixed.
//
// ⚠️ `test/` IS IN SCOPE, AND NOT AS AN AFTERTHOUGHT. The first draft of this
// case walked `src/` alone. Writing the very next line of it put a raw NUL into
// THIS file — copied along with the character being described — and the case sat
// green while the file asserting it was itself binary and unsearchable. A guard
// that cannot see the tree it lives in is one an editing slip switches off.
const SCANNED_ROOTS = ['../src', '../test', '../scripts'] as const;

function sourceFiles(directory: string): string[] {
  const found: string[] = [];
  for (const name of readdirSync(directory)) {
    if (name === 'node_modules') continue;
    const full = join(directory, name);
    if (statSync(full).isDirectory()) found.push(...sourceFiles(full));
    else if (/\.(ts|tsx|js|mjs|json|py)$/.test(name)) found.push(full);
  }
  return found;
}

const REPO_ROOT = fileURLToPath(new URL('..', import.meta.url));

describe('source stays visible to a content search', () => {
  it('no source file contains a raw NUL byte', () => {
    // `scripts/` holds the sabotage runners and exists only on the branches that
    // carry them, so an absent root is tolerated — but ONLY absent. A root that
    // exists and cannot be read is a real error and is left to throw, and the
    // count floor below is what stops "tolerated" from quietly becoming "empty".
    const files = SCANNED_ROOTS.flatMap((root) => {
      const path = fileURLToPath(new URL(root, import.meta.url));
      return existsSync(path) ? sourceFiles(path) : [];
    });
    // Guard the guard: a walk that finds nothing would pass this vacuously, and
    // "0 files scanned" is precisely the shape of the failure being tested for.
    expect(files.length).toBeGreaterThan(20);

    const offenders = files
      .map((file) => ({ file, nulCount: readFileSync(file).filter((b) => b === 0).length }))
      .filter((row) => row.nulCount > 0)
      .map((row) => `${row.file.slice(REPO_ROOT.length)} (${row.nulCount})`);

    expect(offenders, 'write the NUL as the escape \\u0000 — a raw one makes the file binary, and every search tool then reports zero matches in it without saying so').toEqual([]);
  });

  it('the scan can actually see a NUL — it is not passing because the read is wrong', () => {
    // The cry-wolf control. `readFileSync` without an encoding returns a Buffer
    // and the byte survives; with an encoding it would still survive as U+0000.
    // What would break the case above is a walk that reads nothing, so prove the
    // predicate fires on a value known to contain the byte.
    const withNul = Buffer.from([0x61, 0x00, 0x62]);
    expect(withNul.filter((b) => b === 0).length).toBe(1);
  });
});

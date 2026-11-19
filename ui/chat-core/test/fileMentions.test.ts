import { describe, expect, it } from 'vitest';
import { fileMentionOf, filesMatchingMention, type FileNamedByTools } from '../src/reduce/fileMentions.js';
import { remarkRefChips } from '../src/reduce/refChips.js';

function file(path: string): FileNamedByTools {
  return { path, size: 1, modified_at: '2026-09-29T00:00:00Z' };
}

describe('fileMentionOf', () => {
  it('reads the two mentions br_1790637091562322466 wrote', () => {
    expect(fileMentionOf('internal/server/renamer.go')).toEqual({ path: 'internal/server/renamer.go', line: null });
    expect(fileMentionOf('sessions.go:707')).toEqual({ path: 'sessions.go', line: 707 });
  });

  it('takes the first line of a range and of line:column', () => {
    expect(fileMentionOf('a/b.ts:12-40')?.line).toBe(12);
    expect(fileMentionOf('a/b.ts:12:5')?.line).toBe(12);
  });

  it('refuses what is not a file', () => {
    for (const text of ['config.Load', 'v1.2', 'internal/server', 'https://x.com/a.go', 'a b.go', 'br_1234567890123456']) {
      expect(fileMentionOf(text), text).toBeNull();
    }
  });
});

describe('filesMatchingMention', () => {
  const files = [
    file('/r/llm-bridge-server/internal/server/sessions.go'),
    file('/r/llm-bridge-server-wt-x/internal/server/sessions.go'),
    file('/r/llm-bridge-server/internal/server/usersessions.go'),
    file('/home/me/repos/dash/README.md'),
  ];

  it('matches at a directory boundary and returns every match', () => {
    expect(filesMatchingMention(files, 'sessions.go').map((f) => f.path)).toEqual([
      '/r/llm-bridge-server/internal/server/sessions.go',
      '/r/llm-bridge-server-wt-x/internal/server/sessions.go',
    ]);
  });

  it('matches an absolute path exactly, and ~/ and ./ by their tail', () => {
    expect(filesMatchingMention(files, '/r/llm-bridge-server/internal/server/sessions.go')).toHaveLength(1);
    expect(filesMatchingMention(files, '/internal/server/sessions.go')).toHaveLength(0);
    expect(filesMatchingMention(files, '~/repos/dash/README.md')).toHaveLength(1);
    expect(filesMatchingMention(files, './README.md')).toHaveLength(1);
  });
});

describe('remarkRefChips — file chips', () => {
  it('makes a whole code span that is a file path a file chip, and leaves others as code', () => {
    const tree = {
      type: 'root',
      children: [
        {
          type: 'paragraph',
          children: [
            { type: 'inlineCode', value: 'sessions.go:707' },
            { type: 'text', value: ' calls ' },
            { type: 'inlineCode', value: 'config.Load' },
            { type: 'text', value: ' in sessions.go' },
          ],
        },
      ],
    };
    remarkRefChips()(tree as never);
    const kids = (tree.children[0] as { children: Array<{ type: string; data?: { hProperties: unknown } }> }).children;
    expect(kids.map((k) => k.type)).toEqual(['refChip', 'text', 'inlineCode', 'text']);
    expect(kids[0]?.data?.hProperties).toEqual({ kind: 'file', refId: 'sessions.go:707' });
  });
});

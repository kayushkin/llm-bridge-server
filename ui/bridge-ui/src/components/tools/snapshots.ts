// Shared types and helpers for fetching tool-call file snapshots from the
// bridge server. Both EditRenderer and BashRenderer use these to render the
// diff for whichever files a tool call mutated.

// SnapshotMeta mirrors the server's per-phase metadata payload. Either phase
// may be absent if snapshotting didn't fire (binary, oversized, missing,
// etc.); the renderer falls back to whatever inputs the tool itself carried.
export interface SnapshotMeta {
  phase: 'before' | 'after'
  file_path: string
  size: number
  blob_sha?: string
  blob_url?: string
  is_binary?: boolean
  too_large?: boolean
  missing?: boolean
}

// FileSnapshots groups a single file's before/after pair. A tool call can
// produce many of these (Bash) or just one (Edit/Write/MultiEdit/NotebookEdit).
export interface FileSnapshots {
  file_path: string
  before: SnapshotMeta | null
  after: SnapshotMeta | null
}

export interface SnapshotsResponse {
  files: FileSnapshots[]
}

export interface LoadedSide {
  label: string
  content: string
  meta?: SnapshotMeta
}

export interface LoadedFileDiff {
  filePath: string
  before: LoadedSide
  after: LoadedSide
  hint: string
}

// loadSide fetches the blob for one phase, falling back to a caller-supplied
// string when the snapshot is absent or unavailable (binary, oversized,
// missing, no blob_url).
export async function loadSide(
  fetchFn: (url: string, opts?: RequestInit) => Promise<Response>,
  basePath: string,
  label: string,
  meta: SnapshotMeta | null,
  fallback: string | undefined,
): Promise<LoadedSide> {
  if (!meta) {
    return { label, content: fallback ?? '' }
  }
  if (meta.missing || meta.is_binary || meta.too_large || !meta.blob_url) {
    return { label, content: fallback ?? '', meta }
  }
  const res = await fetchFn(`${basePath}${meta.blob_url}`)
  if (!res.ok) throw new Error(`blob ${res.status}`)
  const text = await res.text()
  return { label, content: text, meta }
}

// sidesHint surfaces side-specific degradations (binary, oversized, new file)
// so the UI can explain why a half of the diff is empty.
export function sidesHint(before: SnapshotMeta | null, after: SnapshotMeta | null): string {
  const parts: string[] = []
  if (before?.missing && !after?.missing) parts.push('new file')
  if (after?.missing && !before?.missing) parts.push('file deleted')
  if (before?.is_binary || after?.is_binary) parts.push('binary file')
  if (before?.too_large || after?.too_large) parts.push('file exceeds size cap')
  return parts.join(' · ')
}

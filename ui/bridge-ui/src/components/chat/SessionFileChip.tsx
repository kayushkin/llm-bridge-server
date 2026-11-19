import { createContext, useContext, useSyncExternalStore, useState, type JSX } from 'react'
import { useChatContext, type SessionFile } from '@kayushkin/chat-core'
import { AnchoredPanel, RefChip, timeAgo, truncate, useAnchoredPanel } from './RefChip'
import { sharedFileGalleryKey, useOpenImage, type GalleryImage } from './ImageGallery'
import { formatBytes } from '../../toolPayloadPreview'
import { chatKey } from './storageKeys'
import { parseFoldedSessionFiles, withSessionFileFold } from '../../foldedSessionFiles'
import {
  sessionFileFacts,
  sessionFileGlyph,
  sessionFileKind,
  type SessionFileFact,
} from '../../sessionFileReferences'

/**
 * The reference chip of a file shared into a session, and the fold that puts a
 * picture or video away behind it.
 *
 * Every shared file gets this chip wherever the chat shows it: on its card, under its
 * tile in a group of pictures, where an agent pasted its address as a markdown image
 * or link, and where an agent wrote its name or `file_…` id in prose. The chip's panel
 * says what the record is — its id, kind and size, who shared it into which chat and
 * when, and where it came from when the server recorded that — and offers the viewer
 * and the download. Folding a card leaves only the chip, so nothing folded is lost.
 */

/** One file of the loaded transcript, with the name of the person who shared it when
 *  a person did. */
export interface SessionFileShown {
  file: SessionFile
  /** The transcript's label for the person who shared it; null when the agent did. */
  personName: string | null
}

/** The files shared into the session whose transcript is on screen, by file id.
 *  TurnList provides it; prose outside a transcript has none. */
export const SessionFilesContext = createContext<ReadonlyMap<string, SessionFileShown> | null>(null)

export function useSessionFileShown(fileId: string): SessionFileShown | null {
  return useContext(SessionFilesContext)?.get(fileId) ?? null
}

/** A shared file as the viewer shows it. */
export function galleryImageOfSharedFile(
  api: { sessionFileContentUrl: (sessionId: string, fileId: string, opts?: { inline?: boolean }) => string },
  file: SessionFile,
): GalleryImage {
  return {
    key: sharedFileGalleryKey(file.file_id),
    fileId: file.file_id,
    src: api.sessionFileContentUrl(file.session_id, file.file_id, { inline: true }),
    name: file.filename,
    downloadHref: api.sessionFileContentUrl(file.session_id, file.file_id),
    sizeBytes: file.size_bytes,
    sharedAt: file.created_at,
  }
}

// ---------------------------------------------------------------------------
// Folds, kept in this browser
// ---------------------------------------------------------------------------

const FOLDED_KEY = chatKey('folded-session-files')
const foldListeners = new Set<() => void>()
let foldedKeys: ReadonlySet<string> | null = null

function readFoldedKeys(): ReadonlySet<string> {
  if (foldedKeys === null) {
    let stored: string | null = null
    try {
      stored = window.localStorage.getItem(FOLDED_KEY)
    } catch {
      // Storage switched off: folds last until the page is reloaded.
    }
    foldedKeys = new Set(parseFoldedSessionFiles(stored))
  }
  return foldedKeys
}

function setFolded(key: string, fold: boolean): void {
  const next = withSessionFileFold([...readFoldedKeys()], key, fold)
  foldedKeys = new Set(next)
  try {
    window.localStorage.setItem(FOLDED_KEY, JSON.stringify(next))
  } catch {
    // Storage full or switched off: the fold still holds for this page.
  }
  foldListeners.forEach((listener) => listener())
}

function subscribeToFolds(listener: () => void): () => void {
  foldListeners.add(listener)
  return () => foldListeners.delete(listener)
}

const NO_FOLDS: ReadonlySet<string> = new Set()

/** Whether the thing with this fold key is folded, and the call that folds or
 *  unfolds it. Every card with the same key follows, on screen or drawn later. */
export function useFoldedSessionFile(key: string): { folded: boolean; setFolded: (fold: boolean) => void } {
  const keys = useSyncExternalStore(subscribeToFolds, readFoldedKeys, () => NO_FOLDS)
  return { folded: keys.has(key), setFolded: (fold) => setFolded(key, fold) }
}

/** The button that folds a picture, video or group away, or brings it back. */
export function FoldToggle({
  folded,
  onToggle,
  what,
}: {
  folded: boolean
  onToggle: () => void
  /** What it folds, for the label: "character-lineup.jpg", "6 images". */
  what: string
}): JSX.Element {
  return (
    <button
      type="button"
      className="bc-file-fold"
      onClick={onToggle}
      aria-expanded={!folded}
      aria-label={folded ? `Show ${what}` : `Fold ${what} away`}
      title={folded ? `Show ${what}` : `Fold ${what} away`}
    >
      <span aria-hidden>{folded ? '▸' : '▾'}</span>
    </button>
  )
}

// ---------------------------------------------------------------------------
// The chip and its panel
// ---------------------------------------------------------------------------

/**
 * A shared file's chip. `label` is what the chip reads — the file's name on its own
 * card, the words of the link an agent wrote, the mention as it was written — and
 * the file's id sits beside it unless the label already is the id.
 */
export function SessionFileChip({
  file,
  personName,
  label,
  className,
  showThumbnail = true,
  showId = true,
}: {
  file: SessionFile
  personName: string | null
  label?: string
  className?: string
  /** A small picture on an image's chip; off where the picture is drawn right above. */
  showThumbnail?: boolean
  /** The id on the chip itself; off where room is short. The panel always has it. */
  showId?: boolean
}): JSX.Element {
  const { api } = useChatContext()
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel()
  const kind = sessionFileKind(file.media_type)
  const text = label ?? file.filename
  const inlineSrc = api.sessionFileContentUrl(file.session_id, file.file_id, { inline: true })
  return (
    <span ref={wrapRef} className="ref-chip-wrap" data-ref-kind="session-file" data-ref-id={file.file_id}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-session-file${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${file.filename} (${file.file_id}) — click for where it came from`}
      >
        {kind === 'image' && showThumbnail ? (
          <img className="ref-chip-image-thumb" src={inlineSrc} alt="" loading="lazy" />
        ) : (
          <span className="ref-chip-glyph" aria-hidden>{sessionFileGlyph(kind)}</span>
        )}
        <span className="ref-chip-label">{truncate(text, 48)}</span>
        {showId && text !== file.file_id && <span className="ref-chip-file-id">{file.file_id}</span>}
        <span className="ref-chip-file-caret" aria-hidden="true">{open ? '▾' : '▸'}</span>
      </button>
      {open && (
        <AnchoredPanel
          panelRef={panelRef}
          panelStyle={panelStyle}
          label={`File ${file.filename}`}
          refId={file.file_id}
          refKind="session-file"
        >
          <SessionFilePanel file={file} personName={personName} />
        </AnchoredPanel>
      )}
    </span>
  )
}

/** What the panel shows of one file: the picture or the player, every fact the chat
 *  holds about the record, and the actions. */
function SessionFilePanel({ file, personName }: { file: SessionFile; personName: string | null }): JSX.Element {
  const { api } = useChatContext()
  const kind = sessionFileKind(file.media_type)
  const image = galleryImageOfSharedFile(api, file)
  const viewer = useOpenImage(image)
  // A long page shows its top across the panel's width rather than as a sliver.
  const [tall, setTall] = useState(false)
  const [copied, setCopied] = useState(false)
  const facts = sessionFileFacts(file, { personName, formatBytes })
  const copyId = (): void => {
    void navigator.clipboard?.writeText(file.file_id).then(() => setCopied(true))
  }
  return (
    <div className="ref-chip-panel-session-file">
      {kind === 'image' && (
        <button
          type="button"
          className={`ref-chip-image-preview${tall ? ' ref-chip-image-preview-tall' : ''}`}
          onClick={viewer.open}
          title="Open in the viewer"
        >
          <img
            src={image.src}
            alt={file.filename}
            onLoad={(event) => setTall(event.currentTarget.naturalHeight > event.currentTarget.naturalWidth * 2)}
          />
        </button>
      )}
      {kind === 'video' && (
        <video className="ref-chip-file-video" src={image.src} controls preload="metadata" aria-label={file.filename} />
      )}
      <div className="ref-chip-panel-title">{file.filename}</div>
      {facts.map((fact, position) => (
        <div className="ref-chip-panel-row" key={`${fact.label}:${position}`}>
          <span className="ref-chip-panel-label">{fact.label}</span>
          <span className="ref-chip-panel-value">
            <SessionFileFactValue fact={fact} />
          </span>
        </div>
      ))}
      <div className="ref-chip-image-meta">
        <span className="ref-chip-image-spacer" />
        <button type="button" className="ref-chip-image-action" onClick={copyId}>
          {copied ? 'Copied' : 'Copy id'}
        </button>
        {kind === 'image' && (
          <button type="button" className="ref-chip-image-action" onClick={viewer.open}>View</button>
        )}
        <a className="ref-chip-image-action" href={image.downloadHref} download={file.filename}>Download</a>
      </div>
      {viewer.viewer}
    </div>
  )
}

function SessionFileFactValue({ fact }: { fact: SessionFileFact }): JSX.Element {
  switch (fact.format) {
    case 'session':
      return <RefChip kind="session" refId={fact.value} />
    case 'time':
      return (
        <time dateTime={fact.value} title={fact.value}>
          {timeAgo(fact.value)} · {formatLocalTime(fact.value)}
        </time>
      )
    case 'path':
    case 'id':
      return <code className="ref-chip-file-fact-code">{fact.value}</code>
    case 'text':
      return <>{fact.value}</>
  }
}

/** The time in the reader's own zone; the raw text when it does not parse, never a
 *  made-up time. */
function formatLocalTime(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return iso
  return at.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

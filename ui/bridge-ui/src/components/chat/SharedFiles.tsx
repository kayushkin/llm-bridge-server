import { useState, type ComponentProps, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useChatContext, type ApiClient, type SessionFile, type ToolResultImage } from '@kayushkin/chat-core'
import { formatBytes } from '../../toolPayloadPreview'
import { mayDrawAsImage, mayPlayAsVideo, textPreviewKind, type TextPreviewKind } from '../../sessionFileAttachments'
import { toolImageGalleryKey, useOpenImage, type GalleryImage } from './ImageGallery'
import {
  FoldToggle,
  SessionFileChip,
  galleryImageOfSharedFile,
  useFoldedSessionFile,
  useSessionFileShown,
} from './SessionFileChip'
import { imageGroupFoldKey, sessionFileFoldKey } from '../../foldedSessionFiles'
import { imageGroupLabel } from '../../sessionFileReferences'

export { galleryImageOfSharedFile }

/**
 * Images, videos and files in the chat: a card for a file shared into the session, a
 * compact grid for several images shared together, and the strip of images a tool
 * result carried. Each image opens the viewer in `ImageGallery.tsx`.
 *
 * Every shared file carries its reference chip (`SessionFileChip.tsx`): the chip's
 * panel holds the file's id, who shared it and where it came from. A picture, a video
 * or a whole group folds away behind its chip, and stays folded in this browser.
 *
 * Every byte comes from the bridge through the host's proxy — a shared file from
 * file-store (`ApiClient.sessionFileContentUrl`), a settled tool image from log-store
 * (`ApiClient.toolResultImageUrl`) — or, for a tool image folded live, from the event
 * itself. Both servers send `nosniff` and a sandbox CSP; this layer adds nothing and
 * decides nothing about which types are safe, it only tries to draw what claims to be
 * an image, or play what claims to be a video, and falls back to a link when the
 * browser cannot.
 */

/** A thumbnail that opens the viewer at its image, inside the session's gallery when
 *  the image is in it. `onUndrawable` fires when the browser cannot draw the bytes, so
 *  the caller can show a link instead of a broken image. */
function ImageThumbnail({
  image,
  className = 'bc-image-thumb',
  onUndrawable,
  children,
}: {
  image: GalleryImage
  className?: string
  onUndrawable?: () => void
  children?: ReactNode
}) {
  const { open, viewer } = useOpenImage(image)
  return (
    <>
      <button type="button" className={className} onClick={open} title={`View ${image.name}`}>
        <img src={image.src} alt={image.name} loading="lazy" onError={onUndrawable} />
        {children}
      </button>
      {viewer}
    </>
  )
}

/** An image a tool result carried, as the viewer shows it; null when its bytes are
 *  neither in the event nor addressable yet. */
export function galleryImageOfToolResult(
  api: { toolResultImageUrl: ApiClient['toolResultImageUrl'] },
  sessionId: string | null,
  entryId: string,
  image: ToolResultImage,
  name: string,
): GalleryImage | null {
  const key = toolImageGalleryKey(entryId, image.index)
  if (image.base64Data !== undefined) return { key, src: `data:${image.mediaType};base64,${image.base64Data}`, name }
  if (image.eventId !== undefined && sessionId) {
    const src = api.toolResultImageUrl(sessionId, image.eventId, image.index)
    return { key, src, name, downloadHref: src }
  }
  return null
}

/** Links in a previewed file leave the app in a new tab, so following one does not
 *  lose the chat. */
const PREVIEW_MARKDOWN_COMPONENTS: ComponentProps<typeof ReactMarkdown>['components'] = {
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
}

type PreviewState =
  | { status: 'closed' }
  | { status: 'loading' }
  | { status: 'loaded'; text: string }
  | { status: 'failed'; message: string }

/** A text file's contents, folded under the card until opened. The bytes are fetched
 *  the first time it opens — a transcript with many files should not download them all
 *  — through the host's credentialed fetch, and kept for the life of the card. A
 *  failed fetch says what the server answered; closing and opening again retries it. */
function TextFilePreview({ file, kind }: { file: SessionFile; kind: TextPreviewKind }) {
  const { api } = useChatContext()
  const [state, setState] = useState<PreviewState>({ status: 'closed' })

  const load = async () => {
    setState({ status: 'loading' })
    try {
      const response = await api.fetchFor()(api.sessionFileContentUrl(file.session_id, file.file_id))
      if (!response.ok) {
        const detail = await response.text()
        setState({ status: 'failed', message: `${response.status} ${response.statusText} ${detail}`.trim() })
        return
      }
      setState({ status: 'loaded', text: await response.text() })
    } catch (error) {
      setState({ status: 'failed', message: error instanceof Error ? error.message : String(error) })
    }
  }

  return (
    <details
      className="bc-shared-file-preview"
      onToggle={(event) => {
        if (!event.currentTarget.open) return
        if (state.status === 'closed' || state.status === 'failed') void load()
      }}
    >
      <summary>Preview</summary>
      {state.status === 'loading' && <div className="bc-shared-file-preview-note">Loading…</div>}
      {state.status === 'failed' && (
        <div className="bc-shared-file-preview-note bc-shared-file-preview-error">Could not load: {state.message}</div>
      )}
      {state.status === 'loaded' &&
        (kind === 'markdown' ? (
          <div className="bc-shared-file-preview-body bc-turns-md">
            <ReactMarkdown remarkPlugins={[remarkGfm]} components={PREVIEW_MARKDOWN_COMPONENTS}>
              {state.text}
            </ReactMarkdown>
          </div>
        ) : (
          <pre className="bc-shared-file-preview-body">{state.text}</pre>
        ))}
    </details>
  )
}

/**
 * One file shared into the session: its reference chip, then the picture when it is an
 * image the server serves inline, the player when it is such a video, and a preview to
 * unfold when it is text. The fold button puts the picture or video away and leaves
 * the chip, whose panel still shows it.
 *
 * `inProse` is the card where an agent pasted the file's address as a markdown image:
 * it is built from inline elements only, since it sits inside a paragraph, and it
 * shows the agent's own words for the picture under it.
 */
export function SharedFileCard({
  file,
  inProse = false,
  caption,
}: {
  file: SessionFile
  inProse?: boolean
  /** The agent's description of the picture, from the markdown image's alt text. */
  caption?: string
}) {
  const { api } = useChatContext()
  const [undrawable, setUndrawable] = useState(false)
  const shown = useSessionFileShown(file.file_id)
  const { folded, setFolded } = useFoldedSessionFile(sessionFileFoldKey(file.file_id))
  const image = galleryImageOfSharedFile(api, file)
  const drawn = mayDrawAsImage(file.media_type) && !undrawable
  const played = mayPlayAsVideo(file.media_type) && !undrawable
  const previewKind = inProse ? null : textPreviewKind(file)
  const foldable = drawn || played
  const Block = inProse ? 'span' : 'div'
  return (
    <Block className={`bc-shared-file${inProse ? ' bc-shared-file-in-prose' : ''}`} data-file-id={file.file_id}>
      <span className="bc-shared-file-head">
        {foldable && <FoldToggle folded={folded} onToggle={() => setFolded(!folded)} what={file.filename} />}
        <SessionFileChip file={file} personName={shown?.personName ?? null} showThumbnail={folded} />
        <span className="bc-shared-file-size">{formatBytes(file.size_bytes)}</span>
      </span>
      {drawn && !folded ? (
        <ImageThumbnail image={image} onUndrawable={() => setUndrawable(true)} />
      ) : null}
      {played && !folded ? (
        <video
          className="bc-shared-file-video"
          src={image.src}
          controls
          preload="metadata"
          aria-label={file.filename}
          onError={() => setUndrawable(true)}
        />
      ) : null}
      {caption && !folded ? <span className="bc-shared-file-caption">{caption}</span> : null}
      {previewKind && <TextFilePreview file={file} kind={previewKind} />}
    </Block>
  )
}

/**
 * Several images shared one after another, as one grid of tiles with each image's
 * chip under its tile, rather than a full-size card each down the page. A tile opens
 * the viewer; the chip says where the image came from. Folded, the group is its
 * heading and the row of chips.
 */
export function SharedImageGrid({ files }: { files: readonly SessionFile[] }) {
  const { api } = useChatContext()
  const { folded, setFolded } = useFoldedSessionFile(imageGroupFoldKey(files[0]!.file_id))
  const label = imageGroupLabel(files.length)
  return (
    <div className="bc-image-group" data-folded={folded ? 'true' : 'false'}>
      <div className="bc-image-group-head">
        <FoldToggle folded={folded} onToggle={() => setFolded(!folded)} what={label} />
        <span className="bc-image-group-label">{label}</span>
      </div>
      {folded ? (
        <div className="bc-image-group-chips" role="group" aria-label={label}>
          {files.map((file) => (
            <GridFileChip key={file.file_id} file={file} showThumbnail />
          ))}
        </div>
      ) : (
        <div className="bc-image-grid" role="group" aria-label={label}>
          {files.map((file) => (
            <figure key={file.file_id} className="bc-image-grid-item" data-file-id={file.file_id}>
              <ImageThumbnail image={galleryImageOfSharedFile(api, file)} className="bc-image-grid-tile" />
              <figcaption className="bc-image-grid-caption">
                <GridFileChip file={file} showThumbnail={false} />
              </figcaption>
            </figure>
          ))}
        </div>
      )}
    </div>
  )
}

/** One image's chip in a group: its name alone, since a tile is narrow; the id is on
 *  the chip's title and in its panel. */
function GridFileChip({ file, showThumbnail }: { file: SessionFile; showThumbnail: boolean }) {
  const shown = useSessionFileShown(file.file_id)
  return <SessionFileChip file={file} personName={shown?.personName ?? null} showThumbnail={showThumbnail} showId={false} />
}

/** The images a tool result carried — a screenshot, an image file the agent read. */
export function ToolResultImageStrip({
  sessionId,
  entryId,
  images,
  toolName,
}: {
  sessionId: string | null
  entryId: string
  images: readonly ToolResultImage[]
  toolName: string
}) {
  const { api } = useChatContext()
  const shown = images.flatMap((image, position) => {
    const galleryImage = galleryImageOfToolResult(api, sessionId, entryId, image, `${toolName} image ${position + 1}`)
    return galleryImage ? [galleryImage] : []
  })
  if (shown.length === 0) return null
  return (
    <div className="bc-tool-images">
      {shown.map((image) => (
        <ImageThumbnail key={image.key} image={image} />
      ))}
    </div>
  )
}

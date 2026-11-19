import { createContext, useContext, useEffect, useMemo, useRef, useState, type JSX } from 'react';
import {
  fileMentionOf,
  filesMatchingMention,
  useFileNamedByToolsContent,
  useFilesNamedByTools,
  useChatContext,
  type FileNamedByTools,
} from '@kayushkin/chat-core';
import { AnchoredPanel, timeAgo, useAnchoredPanel } from './RefChip';
import { useOpenImage, type GalleryImage } from './ImageGallery';
import { SessionFileChip, SessionFilesContext, type SessionFileShown } from './SessionFileChip';
import { imageNamedByMention } from '../../sessionImageMentions';
import { sessionFileKind } from '../../sessionFileReferences';
import { formatBytes } from '../../toolPayloadPreview';

/**
 * Where a file chip is: the session whose tools named the files, and when the
 * message holding the chip was written. Prose that is not part of a session's
 * transcript provides none, and its file mentions stay plain code.
 */
export interface FileMentionPlace {
  sessionId: string;
  /** RFC 3339; the file shows as changed when it was modified after this. */
  writtenAt: string | null;
}

export const FileMentionPlaceContext = createContext<FileMentionPlace | null>(null);

/**
 * A file an agent mentioned in a code span (`sessions.go:707`), as a chip that
 * opens the file as it is on disk now.
 *
 * The chip matches the mention against the files the session's own tools named
 * (llm-bridge-server's files-named-by-tools); while that list loads, when it
 * fails, or when nothing on it matches, the mention renders as the plain code
 * span it was written as. When several files match — the same relative path in a
 * main clone and its worktree, usually — the panel lists them and opens none
 * until one is picked.
 */
export function FileRefChip({ mentionText, className }: { mentionText: string; className?: string }): JSX.Element {
  const place = useContext(FileMentionPlaceContext);
  const mention = fileMentionOf(mentionText);
  // A picture or video shared into the session wins: its chip shows it and says where
  // it came from, where the files-named-by-tools chip would show the bytes as text.
  const sessionFiles = useContext(SessionFilesContext);
  const shared = useMemo(
    () => (sessionFiles && mention ? sharedMediaNamedByMention(sessionFiles, mention.path, place?.writtenAt ?? null) : null),
    [sessionFiles, mention?.path, place?.writtenAt],
  );
  if (shared) {
    return <SessionFileChip file={shared.file} personName={shared.personName} label={mentionText} className={className} />;
  }
  return <FileRefChipFromTools mentionText={mentionText} className={className} place={place} mention={mention} />;
}

/** The shared picture or video a mention names, by the rule in
 *  `sessionImageMentions.ts`. Any other kind of shared file is left to the
 *  files-named-by-tools chip, which opens a text file as it is on disk now. */
function sharedMediaNamedByMention(
  sessionFiles: ReadonlyMap<string, SessionFileShown>,
  mentionPath: string,
  writtenAt: string | null,
): SessionFileShown | null {
  const media = [...sessionFiles.values()]
    .filter((shown) => sessionFileKind(shown.file.media_type) !== 'file')
    .map((shown) => ({ key: shown.file.file_id, name: shown.file.filename, sharedAt: shown.file.created_at }));
  const named = imageNamedByMention(media, mentionPath, writtenAt);
  return named ? sessionFiles.get(named.key) ?? null : null;
}

/**
 * A mention of an image that is not a session file record — one the session's tools
 * named, read from disk — as a small thumbnail and the name. It opens a panel with
 * the picture; the picture, or View, opens the viewer at it. An image shared into the
 * session draws as `SessionFileChip` instead, which also says where it came from.
 */
export function ImageRefChip({
  mentionText,
  image,
  className,
}: {
  mentionText: string;
  image: GalleryImage;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const viewer = useOpenImage(image);
  // A long page shows its top across the panel's width rather than as a sliver.
  const [tall, setTall] = useState(false);
  return (
    <span ref={wrapRef} className="ref-chip-wrap" data-ref-kind="image" data-ref-id={image.key}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-image${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${image.name} — click to show`}
      >
        <img className="ref-chip-image-thumb" src={image.src} alt="" loading="lazy" />
        {mentionText}
        <span className="ref-chip-file-caret" aria-hidden="true">{open ? '▾' : '▸'}</span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label={`Image ${image.name}`} refId={image.key} refKind="image">
          <div className="ref-chip-panel-image">
            <button
              type="button"
              className={`ref-chip-image-preview${tall ? ' ref-chip-image-preview-tall' : ''}`}
              onClick={viewer.open}
              title="Open in the viewer"
            >
              <img
                src={image.src}
                alt={image.name}
                onLoad={(event) => setTall(event.currentTarget.naturalHeight > event.currentTarget.naturalWidth * 2)}
              />
            </button>
            <div className="ref-chip-image-meta">
              <span className="ref-chip-image-name">{image.name}</span>
              {image.sizeBytes !== undefined && <span>{formatBytes(image.sizeBytes)}</span>}
              <span className="ref-chip-image-spacer" />
              <button type="button" className="ref-chip-image-action" onClick={viewer.open}>View</button>
              {image.downloadHref && (
                <a className="ref-chip-image-action" href={image.downloadHref} download={image.name}>Download</a>
              )}
            </div>
          </div>
        </AnchoredPanel>
      )}
      {viewer.viewer}
    </span>
  );
}

function FileRefChipFromTools({
  mentionText,
  className,
  place,
  mention,
}: {
  mentionText: string;
  className?: string;
  place: FileMentionPlace | null;
  mention: ReturnType<typeof fileMentionOf>;
}): JSX.Element {
  const files = useFilesNamedByTools(place?.sessionId ?? '');
  const matches = useMemo(
    () => (files.data && mention ? filesMatchingMention(files.data, mention.path) : []),
    [files.data, mention?.path],
  );
  const { api } = useChatContext();
  // The server says how it serves each file (`served_as`): text opens in the panel,
  // an image as a picture, and anything else — another kind, or too large — is left as
  // the plain code it was written as, since no route would open it.
  const texts = matches.filter((match) => match.served_as === 'text');
  const images = matches.filter((match) => match.served_as === 'image');
  const toolImage = useMemo((): GalleryImage | null => {
    const first = images[0];
    if (!first || !place) return null;
    const src = api.fileNamedByToolsImageUrl(place.sessionId, first.path);
    return { key: `files-named-by-tools:${first.path}`, src, name: first.path.split('/').pop() || first.path, sizeBytes: first.size, downloadHref: src };
  }, [images[0]?.path, images[0]?.size, place?.sessionId, api]);
  if (!place || !mention || matches.length === 0) {
    return (
      <code title={files.error ? `could not list this session's files: ${files.error}` : undefined}>{mentionText}</code>
    );
  }
  if (texts.length === 0 && toolImage) {
    return <ImageRefChip mentionText={mentionText} image={toolImage} className={className} />;
  }
  if (texts.length === 0) {
    const kinds = [...new Set(matches.map((match) => match.media_type))].join(', ');
    return <code title={`the chat cannot open this file (${kinds}, ${formatBytes(matches[0]!.size)})`}>{mentionText}</code>;
  }
  return (
    <FileRefChipWithMatches
      mentionText={mentionText}
      line={mention.line}
      matches={texts}
      place={place}
      className={className}
    />
  );
}

function FileRefChipWithMatches({
  mentionText,
  line,
  matches,
  place,
  className,
}: {
  mentionText: string;
  line: number | null;
  matches: FileNamedByTools[];
  place: FileMentionPlace;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const [chosenPath, setChosenPath] = useState<string | null>(null);
  const path = matches.length === 1 ? matches[0]!.path : chosenPath;
  const label = path ? `File ${path}` : `Files matching ${mentionText}`;

  return (
    <span ref={wrapRef} className="ref-chip-wrap" data-ref-kind="file" data-ref-id={mentionText}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-file${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={matches.length === 1 ? matches[0]!.path : `${matches.length} files match — click to choose`}
      >
        <span className="ref-chip-file-caret" aria-hidden="true">{open ? '▾' : '▸'}</span>
        {mentionText}
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label={label} refId={mentionText} refKind="file">
          <div className="ref-chip-panel-file">
            {matches.length > 1 && (
              <div className="ref-chip-file-choices">
                <div className="ref-chip-panel-label">{matches.length} files the session named match</div>
                {matches.map((match) => (
                  <button
                    key={match.path}
                    type="button"
                    className={`ref-chip-file-choice${match.path === path ? ' ref-chip-file-choice-chosen' : ''}`}
                    onClick={() => setChosenPath(match.path)}
                  >
                    {match.path}
                  </button>
                ))}
              </div>
            )}
            {path && <FileContent sessionId={place.sessionId} path={path} line={line} writtenAt={place.writtenAt} />}
          </div>
        </AnchoredPanel>
      )}
    </span>
  );
}

function FileContent({
  sessionId,
  path,
  line,
  writtenAt,
}: {
  sessionId: string;
  path: string;
  line: number | null;
  writtenAt: string | null;
}): JSX.Element {
  const file = useFileNamedByToolsContent(sessionId, path, true);
  const contentRef = useRef<HTMLPreElement | null>(null);
  const targetLineRef = useRef<HTMLDivElement | null>(null);
  // Scroll the file's own box to the line. Not scrollIntoView: that also scrolls
  // every scrolling ancestor, and would move the chat behind the panel.
  useEffect(() => {
    const content = contentRef.current;
    const target = targetLineRef.current;
    if (!content || !target) return;
    content.scrollTop = target.offsetTop - content.clientHeight / 2;
  }, [file.data]);

  if (file.loading) return <div className="ref-chip-panel-loading">Loading {path}…</div>;
  if (file.error || !file.data) return <div className="ref-chip-panel-error">{file.error ?? 'no content'}</div>;

  const changedAfterMessage = writtenAt !== null && Date.parse(file.data.modified_at) > Date.parse(writtenAt);
  const lines = file.data.content.split('\n');
  return (
    <>
      <div className="ref-chip-panel-title ref-chip-file-path">{path}</div>
      <div className="ref-chip-file-meta">
        Current version on disk · changed {timeAgo(file.data.modified_at)}
        {line !== null && ` · line ${line}`}
      </div>
      {changedAfterMessage && (
        <div className="ref-chip-file-changed" role="note">
          This file changed after this message was written, so it may not be what the agent saw.
        </div>
      )}
      <pre ref={contentRef} className="ref-chip-file-content">
        {lines.map((text, index) => {
          const number = index + 1;
          const target = number === line;
          return (
            <div
              key={index}
              ref={target ? targetLineRef : undefined}
              className={`ref-chip-file-line${target ? ' ref-chip-file-line-target' : ''}`}
            >
              <span className="ref-chip-file-line-number">{number}</span>
              {text}
            </div>
          );
        })}
      </pre>
    </>
  );
}

import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ClipboardEvent,
  type CSSProperties,
  type DragEvent,
  type KeyboardEvent,
} from 'react'
import { useChatContext, useConnState, type SessionFile, type useComposer } from '@kayushkin/chat-core'
import { composerAutoGrowHeightPx } from './composerAutoGrow'
import { filesFromPaste, messageWithAttachedFiles, nameForPastedFile } from '../../sessionFileAttachments'
import { formatBytes } from '../../toolPayloadPreview'
import { sentBubblePlacement, sentBubbleText } from '../../composerSentBubble'
import { useMinimalChrome } from '../minimal/MinimalChromeContext'

/** True when the main pointer is a finger, so focusing a text box raises an
 *  on-screen keyboard. */
function touchIsMainPointer(): boolean {
  return typeof window !== 'undefined' && window.matchMedia('(pointer: coarse)').matches
}

/** A file waiting in the composer to go out with the next send. `previewUrl` is an
 *  object URL for an image, revoked when the file leaves the tray. */
interface PendingAttachment {
  key: string
  file: File
  name: string
  previewUrl: string | null
}

let nextAttachmentKey = 0
let nextSentBubbleKey = 0

interface ComposerProps {
  sessionId: string | null
  /** True while the SESSION is producing output, as the server reports it (the
   *  STREAMING_STATES set in Chat). This is not `composer.sending`, which
   *  only means "my own POST /send has not returned yet" and clears in about a
   *  second — reading it as "a turn is running" is what made Stop a flicker. */
  turnRunning: boolean
  /** The pane's ONE `useComposer` instance, owned by Chat — its `error` is
   *  hook-local, and the turns pane's status slot renders it, so both must read
   *  the same instance (the `useSessionControls` rule again). */
  composer: ReturnType<typeof useComposer>
  /** Tells Chat which action `composer.error` now describes, so the status slot
   *  can phrase it ("couldn't stop — still running: …"). null clears the phrasing
   *  when a new action starts. */
  onFailedAction: (action: 'stop' | 'resume' | null) => void
}

/** Draft + optimistic send for the active (or pending/new) session. Enter sends,
 *  Shift+Enter inserts a newline. Mirrors bridge-ui's Composer DOM (bc-composer-wrap /
 *  bc-composer / bc-composer-input / bc-composer-actions / bc-composer-btn /
 *  bc-btn-stop) so it inherits the shared stylesheet.
 *
 *  Stop sits BESIDE Send rather than replacing it. The two verbs are not alternatives:
 *  a running turn is exactly when a user most often wants to redirect the model, and an
 *  exclusive ternary made Send unreachable for the whole turn. Submitting mid-turn
 *  interrupts first and then sends.
 *
 *  Which turn is "running" comes from `turnRunning` (the server-reported session state),
 *  NOT from `useComposer().sending`. `sending` is this client's own in-flight POST and
 *  clears in about a second, so a Stop button keyed on it appeared for a blink at the
 *  start of a turn and was gone for all the minutes the user might actually want it.
 *
 *  **The order is load-bearing.** `send()` is fire-and-forget optimistic (it does not
 *  return a promise), so the interrupt has to be awaited BEFORE it or the two race and
 *  the new message can reach the harness while the old turn still owns it.
 *
 *  `stop()` is a LOUD control (chat-core contract): it throws on a non-2xx (e.g. the
 *  409 the server returns while a tool still holds the turn) and sets `error` — it
 *  never optimistically fakes idle. We surface that failure inline instead of
 *  swallowing it, and a submit whose interrupt failed does NOT go on to send: the turn
 *  is demonstrably still running, so sending anyway is the race the await exists to stop.
 *
 *  Send is also refused while the session-list stream is not open (`useConnState`). The
 *  POST would still be accepted by the server, but nothing would carry the reply back,
 *  so the message would look lost.
 *
 *  Resume appears when the session's harness process is gone (`resumable` — see
 *  RESUMABLE_STATES in chat-core). It is NOT keyed on `paused`: nothing on this box
 *  emits `msg.SessionPaused`, so the "⏸ paused" label this component used to carry on
 *  its own had never once rendered, and a button behind the same flag would have been
 *  the same dead code with a click handler. `resume()` is LOUD like `stop()` — a
 *  refusal is shown, never swallowed into a fake-revived session. */
export default function Composer({ sessionId, turnRunning, composer, onFailedAction }: ComposerProps) {
  const { send, draft, setDraft, sending, stop, interrupting, resume, resuming, resumable } =
    composer
  const connState = useConnState()
  // 'open' is the only state in which updates are actually flowing: a dropped stream
  // goes back to 'connecting' for its backoff, and 'idle' is the pre-start window. So
  // the test is `=== 'open'`, never `!== 'closed'`.
  const connected = connState === 'open'
  const ref = useRef<HTMLTextAreaElement>(null)
  const { api } = useChatContext()
  const filePicker = useRef<HTMLInputElement>(null)
  const [attachments, setAttachments] = useState<PendingAttachment[]>([])
  const [uploading, setUploading] = useState(false)
  const [attachError, setAttachError] = useState<string | null>(null)
  // The message the last send took out of the box, for a host theme to animate leaving
  // (`.bc-composer-sent`, hidden by bridge-ui's stylesheet). It goes when its animation
  // ends, or is replaced by the next send's.
  const wrapRef = useRef<HTMLDivElement>(null)
  const [sentBubble, setSentBubble] = useState<{ key: number; text: string; placement: Record<string, string> } | null>(null)
  const showSentBubble = (text: string) => {
    const textBox = ref.current
    const wrapper = wrapRef.current
    if (!textBox || !wrapper) return
    setSentBubble({
      key: nextSentBubbleKey++,
      text,
      placement: sentBubblePlacement(textBox.getBoundingClientRect(), wrapper.getBoundingClientRect()),
    })
  }

  // True while a send from a new chat is creating its session and sharing files into
  // it. That send moves this pane onto the new session, and the files in the tray are
  // on their way into it, so the switch below must not drop them.
  const sendingFromNewChat = useRef(false)

  // Attachments belong to the session they were picked for. Switching away drops
  // them rather than sending them into whichever chat is opened next.
  useEffect(() => {
    return () => {
      if (sendingFromNewChat.current) return
      setAttachments((current) => {
        for (const attachment of current) {
          if (attachment.previewUrl) URL.revokeObjectURL(attachment.previewUrl)
        }
        return []
      })
      setAttachError(null)
    }
  }, [sessionId])

  const addFiles = (files: readonly File[], pasted: boolean) => {
    if (files.length === 0) return
    const now = new Date()
    setAttachError(null)
    setAttachments((current) => [
      ...current,
      ...files.map((file) => ({
        key: `attachment-${nextAttachmentKey++}`,
        file,
        name: pasted ? nameForPastedFile(file, now) : file.name,
        previewUrl: file.type.startsWith('image/') ? URL.createObjectURL(file) : null,
      })),
    ])
  }

  const removeAttachment = (key: string) => {
    setAttachments((current) => {
      const removed = current.find((attachment) => attachment.key === key)
      if (removed?.previewUrl) URL.revokeObjectURL(removed.previewUrl)
      return current.filter((attachment) => attachment.key !== key)
    })
  }

  // A file is shared into a session, and a new chat has none until its first send
  // creates it. The tray holds the files until then; `submit` shares them once the
  // session exists.
  const onPaste = (event: ClipboardEvent<HTMLTextAreaElement>) => {
    const { files, keepText } = filesFromPaste({
      files: event.clipboardData.files,
      types: Array.from(event.clipboardData.types),
    })
    if (!keepText) event.preventDefault()
    addFiles(files, true)
  }

  const onDrop = (event: DragEvent<HTMLDivElement>) => {
    if (event.dataTransfer.files.length === 0) return
    event.preventDefault()
    addFiles(Array.from(event.dataTransfer.files), false)
  }
  // Which-action-failed phrasing lives in Chat now (`onFailedAction`), because the
  // message renders in the turns pane's status slot, not here — this component only
  // reports which of its buttons the hook's `error` belongs to.

  // Focus the composer once per session, when the active session changes, so opening
  // a chat on a desktop lands the cursor in the input. Keyed on the session id (not
  // every render) so it doesn't steal focus back mid-typing.
  //
  // Not on a phone: focus there raises the on-screen keyboard, which covers half the
  // screen — over the session drawer while it is open, and over the thread the person
  // just opened to read. A phone is the minimal layout, or a touch screen as the main
  // pointer when the person forced the full layout; there the box waits for a tap.
  const { minimal } = useMinimalChrome()
  const focusedForSession = useRef<string | null>(null)
  useEffect(() => {
    if (sessionId && focusedForSession.current !== sessionId) {
      focusedForSession.current = sessionId
      if (minimal || touchIsMainPointer()) return
      ref.current?.focus()
    }
  }, [sessionId, minimal])

  // Auto-grow, so a long message is not composed through a one-line slit. Two details
  // are load-bearing:
  //
  //  - The reset to `0px` first. `scrollHeight` never reports smaller than the box it
  //    is measuring, so without the reset the textarea only ever grows and never comes
  //    back down when the draft is deleted.
  //  - `useLayoutEffect`, not `useEffect`. The measure-and-resize has to happen before
  //    the browser paints, or every keystroke that changes the line count paints once
  //    at the old height first and the box visibly jumps. ⚠️ Nothing in
  //    `e2e/chat-composer-autogrow.spec.ts` catches this one: swapping in `useEffect`
  //    was tried and the whole spec stayed green, because the difference is a single
  //    frame and every assertion there reads a settled height. Do not read the green
  //    suite as permission to change it.
  //
  // The CAP is not written here. `.bc-composer-input` carries `max-height: 220px` in
  // bridge-ui's stylesheet, which this page loads, so the browser clamps the inline
  // height we set and `overflow-y: auto` takes over past the cap. bridge-ui's own
  // Composer duplicates that number as `MAX_INPUT_PX` and then has to keep the two in
  // step by hand; reading nothing and letting the stylesheet decide leaves the cap in
  // one place.
  //
  // `scrollHeight` excludes the border, and dash sets `box-sizing: border-box` on
  // everything (`src/index.css:55`), so the height we assign has to add the border back
  // or the box lands a border-width short of its own content and scrolls by that much
  // forever. The correction lives in bridge-ui as `composerAutoGrowHeightPx` and is
  // called rather than repeated: this page derived it independently, and the two other
  // composers on this fleet that derived it independently both got it wrong.
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const computed = window.getComputedStyle(el)
    el.style.height = '0px'
    el.style.height = `${composerAutoGrowHeightPx({
      scrollHeight: el.scrollHeight,
      boxSizing: computed.boxSizing,
      borderTopWidth: computed.borderTopWidth,
      borderBottomWidth: computed.borderBottomWidth,
    })}px`
  }, [draft])

  const doStop = async () => {
    onFailedAction(null)
    try {
      await stop()
    } catch {
      // The hook already set `error` and did NOT mark the session idle. We only
      // report that this was a stop failure, for the status slot's phrasing.
      onFailedAction('stop')
    }
  }

  const doResume = async () => {
    onFailedAction(null)
    try {
      await resume()
    } catch {
      // LOUD, like stop(): the hook set `error` and did not pretend the session is
      // back. A 409 here means the process turned out to be alive after all; a 500
      // means the session is bound to no instance and cannot be respawned.
      onFailedAction('resume')
    }
  }

  const submit = async () => {
    const hasContent = draft.trim() !== '' || attachments.length > 0
    if (!hasContent || !connected || interrupting || sending || uploading) return
    // Clear the phrasing up front. `error` is now also set by a failed SEND — it
    // used to be swallowed — and a stale flag from an earlier stop or resume would
    // label the send's own message "couldn't stop".
    onFailedAction(null)
    // Mid-turn submit: interrupt, and only send once the interrupt has landed.
    if (turnRunning) {
      try {
        await stop()
      } catch {
        // A stop that failed leaves the turn running. Sending now is the race the
        // await is here to prevent, so stop at the error the hook already surfaced.
        onFailedAction('stop')
        return
      }
      onFailedAction(null)
    }
    if (attachments.length === 0) {
      showSentBubble(sentBubbleText(draft, []))
      await send(draft)
      return
    }
    // Share first, then send one message naming every file. The files go into the
    // session the send targets, which on a new chat exists only once `send` has
    // created it — so the sharing runs inside `send`, as its `prepareMessage`. A
    // failed share sends nothing and keeps the draft and the tray: half the files and
    // a message that names only some of them is worse than a second try.
    const toShare = attachments
    const typed = draft
    showSentBubble(sentBubbleText(typed, toShare.map((attachment) => attachment.name)))
    setUploading(true)
    setAttachError(null)
    sendingFromNewChat.current = sessionId === null
    try {
      await send(typed, {
        prepareMessage: async (targetSessionId) => {
          const shared: SessionFile[] = []
          try {
            for (const attachment of toShare) {
              shared.push(await api.shareSessionFile(targetSessionId, attachment.file, attachment.name))
            }
          } catch (error) {
            const sharedCount = shared.length
            setAttachError(
              `${error instanceof Error ? error.message : String(error)}` +
                (sharedCount > 0 ? ` — ${sharedCount} of ${toShare.length} were shared before it failed, and nothing was sent` : ''),
            )
            // The ones that did go up are in the session already; keep only the rest.
            setAttachments((current) => current.slice(sharedCount))
            throw error
          }
          for (const attachment of toShare) {
            if (attachment.previewUrl) URL.revokeObjectURL(attachment.previewUrl)
          }
          setAttachments([])
          return messageWithAttachedFiles(typed, shared)
        },
      })
    } finally {
      sendingFromNewChat.current = false
      setUploading(false)
    }
  }

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      void submit()
    }
  }

  return (
    <div ref={wrapRef} className="bc-composer-wrap" onDragOver={(event) => event.preventDefault()} onDrop={onDrop}>
      {sentBubble && (
        <div
          key={sentBubble.key}
          className="bc-composer-sent"
          aria-hidden
          style={sentBubble.placement as CSSProperties}
          onAnimationEnd={(event) => {
            if (event.target === event.currentTarget) setSentBubble(null)
          }}
        >
          {sentBubble.text}
        </div>
      )}
      {/* No status row here anymore: paused/stopped/disconnected/error render in the
          turns pane's SessionStatusLine — one slot, one style, no second thing popping
          layout above the composer. The attachment tray is the exception: an upload
          refusal belongs to the files it is about, beside them. */}
      {(attachments.length > 0 || attachError) && (
        <div className="bc-attach-tray">
          {attachments.map((attachment) => (
            <div key={attachment.key} className="bc-attach-chip" title={attachment.name}>
              {attachment.previewUrl ? (
                <img className="bc-attach-chip-preview" src={attachment.previewUrl} alt="" />
              ) : (
                <span aria-hidden>📄</span>
              )}
              <span className="bc-attach-chip-name">{attachment.name}</span>
              <span className="bc-attach-chip-size">{formatBytes(attachment.file.size)}</span>
              <button
                type="button"
                className="bc-attach-chip-remove"
                onClick={() => removeAttachment(attachment.key)}
                disabled={uploading}
                aria-label={`Remove ${attachment.name}`}
              >
                ✕
              </button>
            </div>
          ))}
          {attachError && <div className="bc-attach-error">Couldn’t share: {attachError}</div>}
        </div>
      )}
      <div className="bc-composer">
        <input
          ref={filePicker}
          type="file"
          multiple
          hidden
          onChange={(event) => {
            addFiles(Array.from(event.target.files ?? []), false)
            // Cleared so picking the same file again still fires onChange.
            event.target.value = ''
          }}
        />
        <button
          type="button"
          className="bc-composer-attach"
          onClick={() => filePicker.current?.click()}
          disabled={uploading}
          aria-label="Attach files"
          title="Attach files — or paste or drop them here"
        >
          <AttachIcon />
        </button>
        {/* The textarea stays editable while disconnected: only the send is impossible,
            and disabling the box would throw away a draft over a reconnect that usually
            lasts a second. */}
        <textarea
          ref={ref}
          className="bc-composer-input"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          placeholder={connected ? 'Send a message...' : 'Waiting for the connection…'}
          rows={1}
        />
        <div className="bc-composer-actions">
          <button
            className="bc-composer-btn bc-composer-send"
            onClick={() => void submit()}
            aria-label={uploading ? 'Sharing…' : 'Send'}
            disabled={(!draft.trim() && attachments.length === 0) || !connected || interrupting || sending || uploading}
            title={
              !connected
                ? 'Not connected'
                : turnRunning
                  ? 'Send (interrupts the running turn first)'
                  : 'Send'
            }
          >
            <SendIcon />
            <span className="bc-composer-btn-label">{uploading ? 'Sharing…' : 'Send'}</span>
          </button>
          {turnRunning && (
            <button
              className="bc-composer-btn bc-btn-stop"
              onClick={() => void doStop()}
              disabled={interrupting}
              title="Interrupt the running turn"
              aria-label={interrupting ? 'Stopping…' : 'Stop'}
            >
              <StopIcon />
              <span className="bc-composer-btn-label">{interrupting ? 'Stopping…' : 'Stop'}</span>
            </button>
          )}
          {/* Resume sits BESIDE Send rather than replacing it, for the same reason Stop
              does: sending to a stopped session already revives it (the server starts a
              process when its registry has none), so hiding Send here would remove the
              shorter path to the same place. Resume is for bringing a session back
              WITHOUT putting words in its mouth. */}
          {resumable && (
            <button
              className="bc-composer-btn bc-btn-resume"
              onClick={() => void doResume()}
              disabled={resuming}
              title="Start this session's harness process again"
              aria-label={resuming ? 'Resuming…' : 'Resume'}
            >
              <ResumeIcon />
              <span className="bc-composer-btn-label">{resuming ? 'Resuming…' : 'Resume'}</span>
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

// Each action button carries an icon and a word. The stylesheet shows the word on a
// wide screen and only the icon on a phone, where the text box needs the width.
const composerIconProps = { className: 'bc-composer-btn-icon', viewBox: '0 0 24 24', 'aria-hidden': true } as const

function AttachIcon() {
  return (
    <svg {...composerIconProps} fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round">
      <path d="M12 5v14M5 12h14" />
    </svg>
  )
}

function SendIcon() {
  return (
    <svg {...composerIconProps} fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 19V5M5.5 11.5L12 5l6.5 6.5" />
    </svg>
  )
}

function StopIcon() {
  return (
    <svg {...composerIconProps} fill="currentColor">
      <rect x="6.5" y="6.5" width="11" height="11" rx="2" />
    </svg>
  )
}

function ResumeIcon() {
  return (
    <svg {...composerIconProps} fill="currentColor">
      <path d="M8 5.5v13l10.5-6.5z" />
    </svg>
  )
}

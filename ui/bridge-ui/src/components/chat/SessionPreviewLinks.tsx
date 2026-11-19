import { Link } from 'react-router-dom'
import { useBridgeConfig } from '../../context'
import { previewsOfSession } from '../../devServerPreviews'
import { useDevServerPreviews } from '../../useDevServerPreviews'

/** A "View :port" link in the session header for each server this session's
 *  agent is running. The chat's default view draws no tool calls, so a link
 *  on the Bash call alone would be seen only in Raw; this one is always
 *  there while the server listens. Draws nothing on a host with no preview
 *  list. */
export function SessionPreviewLinks({ sessionId }: { sessionId: string }) {
  const { routes } = useBridgeConfig()
  const { previews } = useDevServerPreviews()
  const own = previewsOfSession(previews, sessionId).filter(preview => preview.preview_port)
  if (own.length === 0) return null
  return (
    <>
      {own.map(preview => (
        <Link
          key={preview.port}
          className="bc-tool-preview-link"
          to={`${routes.previews}?port=${preview.port}`}
          title={`Show the server this session runs on port ${preview.port} (${preview.working_directory})`}
        >
          View :{preview.port}
        </Link>
      ))}
    </>
  )
}

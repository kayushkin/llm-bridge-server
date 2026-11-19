import { useEffect, useRef, useState } from 'react'
import type { ToolEvent } from '../../types'
import ToolItem from './ToolItem'

export default function ToolsSection({ tools, turnDone }: { tools: ToolEvent[]; turnDone: boolean }) {
  const [collapsed, setCollapsed] = useState(true)
  const prevTurnDone = useRef(turnDone)

  useEffect(() => {
    if (turnDone && !prevTurnDone.current) setCollapsed(true)
    prevTurnDone.current = turnDone
  }, [turnDone])

  return (
    <div className={`bc-tools-section${collapsed ? ' bc-tools-section-collapsed' : ''}`}>
      <button
        type="button"
        className="bc-tools-section-header"
        onClick={() => setCollapsed(c => !c)}
      >
        <span className="bc-tools-section-chevron">{collapsed ? '▸' : '▾'}</span>
        <span>Tools ({tools.length})</span>
      </button>
      {!collapsed && (
        <div className="bc-msg-tools">
          {tools.map((t, ti) => (
            <ToolItem key={ti} tool={t} running={false} turnDone={turnDone} />
          ))}
        </div>
      )}
    </div>
  )
}

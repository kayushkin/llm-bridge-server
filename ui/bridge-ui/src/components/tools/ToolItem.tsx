import { useEffect, useRef, useState } from 'react'
import type { ToolEvent } from '../../types'
import { getToolRenderer } from './registry'
import DefaultRenderer from './DefaultRenderer'

export default function ToolItem({ tool, running, turnDone = false }: { tool: ToolEvent; running: boolean; turnDone?: boolean }) {
  const Renderer = getToolRenderer(tool.tool) ?? DefaultRenderer
  const [collapsed, setCollapsed] = useState(true)
  const prevTurnDone = useRef(turnDone)

  useEffect(() => {
    if (turnDone && !prevTurnDone.current) setCollapsed(true)
    prevTurnDone.current = turnDone
  }, [turnDone])

  const cls = `bc-tool-wrap bc-tool-collapsible${collapsed ? ' bc-tool-collapsed' : ''}`
  return (
    <div className={cls} onClick={() => setCollapsed(c => !c)}>
      <Renderer tool={tool} running={running} />
    </div>
  )
}

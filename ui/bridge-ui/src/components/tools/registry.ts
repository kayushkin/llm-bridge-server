import type { FC } from 'react'
import type { ToolRendererProps } from './types'

const registry = new Map<string, FC<ToolRendererProps>>()

export function registerToolRenderer(toolName: string, component: FC<ToolRendererProps>) {
  registry.set(toolName, component)
}

export function getToolRenderer(toolName: string): FC<ToolRendererProps> | undefined {
  return registry.get(toolName)
}

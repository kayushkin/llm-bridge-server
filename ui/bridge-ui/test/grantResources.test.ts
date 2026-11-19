import { describe, expect, it } from 'vitest'
import { compareOptionsByLabel, toolOption, type ToolRecord } from '../src/grantResources'

// tool-store omits `display_name` when it is empty (`json:"display_name,omitempty"`),
// and on 2026-09-11 eleven of its fifteen tools had none. The Grants picker
// labeled tools by `display_name`, so sorting the catalog threw
// "Cannot read properties of undefined (reading 'localeCompare')".

describe('toolOption', () => {
  it('labels a tool with no display_name by its name', () => {
    const tool: ToolRecord = { id: 5, name: 'read_files', kind: 'inprocess', enabled: false }
    expect(toolOption(tool)).toEqual({ id: '5', label: 'read_files', detail: 'inprocess', disabled: true })
  })

  it('shows a display_name beside the kind', () => {
    const tool: ToolRecord = { id: 12, name: 'brave-search', display_name: 'Brave Search', kind: 'mcp', enabled: true }
    expect(toolOption(tool)).toEqual({ id: '12', label: 'brave-search', detail: 'mcp · Brave Search', disabled: false })
  })

  it('sorts a catalog that mixes tools with and without a display_name', () => {
    const options = [
      toolOption({ id: 13, name: 'playwright', display_name: 'Playwright', kind: 'mcp', enabled: true }),
      toolOption({ id: 1, name: 'browser', kind: 'inprocess', enabled: false }),
      toolOption({ id: 12, name: 'brave-search', display_name: 'Brave Search', kind: 'mcp', enabled: true }),
    ]
    expect(options.sort(compareOptionsByLabel).map(option => option.label)).toEqual(['brave-search', 'browser', 'playwright'])
  })
})

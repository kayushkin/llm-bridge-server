import { useEffect, useRef, useState } from 'react'

export interface EditableNameProps {
  value: string
  onSave: (name: string) => void
  className?: string
}

export function EditableName({ value, onSave, className }: EditableNameProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => { if (editing) inputRef.current?.focus() }, [editing])
  useEffect(() => { setDraft(value) }, [value])

  const commit = () => {
    const trimmed = draft.trim()
    if (trimmed && trimmed !== value) onSave(trimmed)
    setEditing(false)
  }

  if (!editing) {
    return <span className={className} onDoubleClick={() => setEditing(true)} title={value}>{value}</span>
  }

  return (
    <input
      ref={inputRef}
      className="bc-inline-edit"
      value={draft}
      onChange={e => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={e => { if (e.key === 'Enter') commit(); if (e.key === 'Escape') setEditing(false) }}
    />
  )
}

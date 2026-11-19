import type { MultichatTag } from '../../types-multichat'
import styles from './Messages.module.css'

/** What a Messages page shows on a host that does not proxy multichat. */
export function MultichatNotConfigured({ page }: { page: string }) {
  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h2 className={styles.title}>{page}</h2>
      </header>
      <div className={styles.empty}>
        This host does not proxy multichat (<code>multichatBasePath</code> is empty), so there is nothing to show.
      </div>
    </div>
  )
}

/** A tag drawn in its own color, as a border and a dot, so it reads on either
 *  theme whatever color it was given. */
export function TagChip({ tag, onRemove, disabled }: { tag: MultichatTag; onRemove?: () => void; disabled?: boolean }) {
  return (
    <span className={styles.tag} style={{ borderColor: tag.color }} data-tag-id={tag.id}>
      <span className={styles.tagDot} style={{ background: tag.color }} />
      {tag.name}
      {onRemove && (
        <button type="button" className={styles.tagRemove} disabled={disabled} title={`Take "${tag.name}" off`} onClick={onRemove}>×</button>
      )}
    </span>
  )
}

export function TagChips({ tags }: { tags: readonly MultichatTag[] }) {
  if (tags.length === 0) return null
  return <>{tags.map(tag => <TagChip key={tag.id} tag={tag} />)}</>
}

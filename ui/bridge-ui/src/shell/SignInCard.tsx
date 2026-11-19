import type { ReactNode } from 'react'
import styles from './SignInCard.module.css'

export interface SignInCardProps {
  title: ReactNode
  /** The line under the title. */
  prompt?: ReactNode
  /** The last refusal, shown as it came. */
  error?: string
  /** Drawn beside the card, such as the zine theme's poster. */
  beside?: ReactNode
  children: ReactNode
}

/** The frame of a sign-in page: a centered card with a title, a prompt, the
 *  last error and the host's own fields and buttons. dash draws its token form
 *  in it, and `BridgeSignInGate` the bridge's own sign-in. */
export function SignInCard({ title, prompt, error, beside, children }: SignInCardProps) {
  return (
    <div className={styles.page}>
      {beside}
      <div className={styles.card}>
        <h2 className={styles.title}>{title}</h2>
        {prompt && <p className={styles.prompt}>{prompt}</p>}
        {error && <p className={styles.error} role="alert">{error}</p>}
        {children}
      </div>
    </div>
  )
}

/** The class names a host's own fields use to match the card. */
export const signInCardClassNames = {
  input: styles.input,
  submit: styles.submit,
  prompt: styles.prompt,
  divider: styles.divider,
  link: `${styles.submit} ${styles.link}`,
}

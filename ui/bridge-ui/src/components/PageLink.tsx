import type { JSX, ReactNode } from 'react'
import { Link, useInRouterContext } from 'react-router-dom'
import { isOffSiteHref } from '../referenceChips'

/**
 * A link to a page: another site's in a new tab, and one of this site's
 * through the router.
 *
 * A path on this site (`/principals?id=…`, `/?session=…`) is written from the
 * root of the host's pages, as reference-store's `page_url`s and this library's
 * routes are. A plain `<a href>` sends it to the root of the origin, which is
 * right for a host that mounts its pages there (dash) and wrong for one that
 * mounts them under a base: llm-bridge-server serves them at `/ui/`, so
 * `/principals` there is its JSON route, not the page. The router adds its
 * basename, so the same path works on both.
 *
 * Outside a router (a chip rendered on its own) there is no basename to add,
 * and the path is used as written.
 */
export function PageLink({ href, className, children }: { href: string; className?: string; children: ReactNode }): JSX.Element {
  const inRouter = useInRouterContext()
  if (isOffSiteHref(href)) {
    return <a className={className} href={href} target="_blank" rel="noreferrer">{children}</a>
  }
  if (!inRouter) return <a className={className} href={href}>{children}</a>
  return <Link className={className} to={href}>{children}</Link>
}

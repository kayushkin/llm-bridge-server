// mailstack's wire shape as the kanban card drawer reads it. Hand-written until
// mailstack renders its own types the way kanban-store and noteboard do.

/** Minimal shape of a mailstack message, as the card drawer reads it.
 *
 * Deliberately partial: this mirrors only the fields the drawer renders, and
 * body_html is omitted on purpose so no caller here can reach for it. Mail
 * bodies are attacker-controlled, and the only surface that renders them safely
 * is dash's Mail page, which sandboxes them in an iframe with remote images
 * stripped. */
export interface MailMessage {
  meta?: {
    subject?: string
    snippet?: string
    date?: string
    from?: { name?: string; email?: string }
  }
  body_text?: string
}

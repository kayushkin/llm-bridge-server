// Package bridgestores names where each store a job binary reads and writes
// answers: a path under llm-bridge-server's one address (LLM_BRIDGE_URL).
//
// Each store keeps its own paths behind its prefix, as the bridge mounts it:
// the boards and cards of issues are /issues/boards and /issues/cards/{id},
// the content of every issue /issues/items, notes /notes/items, and the
// scheduler, model-store, usage-store, principal-store and mailstack their
// standalone paths behind /scheduler, /models, /usage, /principals and /mail.
// The bridge's own routes (/sessions, /oneshot, /operations, /harnesses,
// /instances) are at its root.
package bridgestores

import "strings"

// The prefix each store is mounted under in llm-bridge-server.
const (
	IssuesPath     = "/issues"
	NotesPath      = "/notes"
	SchedulerPath  = "/scheduler"
	ModelsPath     = "/models"
	UsagePath      = "/usage"
	PrincipalsPath = "/principals"
	MailPath       = "/mail"
)

func under(bridgeURL, path string) string {
	return strings.TrimRight(bridgeURL, "/") + path
}

// IssuesURL is where issues answer: its boards, cards and entities, and its
// items (every todo and card's content) at /items.
func IssuesURL(bridgeURL string) string { return under(bridgeURL, IssuesPath) }

// NotesURL is where notes answer: plain notes, ranked lists and workspaces,
// at /items.
func NotesURL(bridgeURL string) string { return under(bridgeURL, NotesPath) }

// SchedulerURL is where the scheduler answers: /api/jobs and /settings.
func SchedulerURL(bridgeURL string) string { return under(bridgeURL, SchedulerPath) }

// ModelsURL is where model-store answers: /api/roles.
func ModelsURL(bridgeURL string) string { return under(bridgeURL, ModelsPath) }

// UsageURL is where usage-store answers: /api/usage/limits.
func UsageURL(bridgeURL string) string { return under(bridgeURL, UsagePath) }

// PrincipalsURL is where principal-store answers: /contacts/resolve.
func PrincipalsURL(bridgeURL string) string { return under(bridgeURL, PrincipalsPath) }

// MailURL is where mailstack answers: /api/accounts, /api/messages and the
// rest of its own paths.
func MailURL(bridgeURL string) string { return under(bridgeURL, MailPath) }

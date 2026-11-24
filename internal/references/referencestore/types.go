package referencestore

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The reference types: every kind of record whose id can be written in prose
// and drawn as a chip. This list is the one place a type is declared; kanban-store,
// journal-store, prediction-store and every chat surface read it from GET /types
// rather than keep a copy.
//
// A row says four things:
//   - how to recognize an id (IDPatterns, matched against the whole id,
//     case-insensitively) and where to fetch one (Service + Get);
//   - how to name it (LabelField) and what to show beside the name (Summary);
//   - where its page is (Page, on PageSite or on the page that draws the chip);
//   - who may see it (Access).
//
// Presentation here is only field names and format hints. The record itself is
// passed through unchanged; the chip formats it.

// UUIDPattern matches the canonical 8-4-4-4-12 hex form, the id shape
// noteboard hands out.
const UUIDPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// PrincipalIDPattern is principal-store's id shape.
const PrincipalIDPattern = `principal_\d{6,}`

// Access says who decides whether a caller may see a record of a type.
type Access string

const (
	// AccessByGrant: an administrator, or a caller holding can_see_references
	// on the type (reference_type <type> or *) or on the one record
	// (reference <id>) in grant-store.
	AccessByGrant Access = "grant"
	// AccessByOwner: the owning service decides. It is asked as the caller
	// (X-Principal-Id), and a refusal (403 or 404) is a refusal here.
	AccessByOwner Access = "owner"
)

// Credential is what reference-store presents to the owning service.
type Credential string

const (
	// CredentialNone: the service answers loopback callers without one.
	CredentialNone Credential = ""
	// CredentialAuthStore: the service's provider in auth_store_providers
	// holds an api_key, sent as a bearer token.
	CredentialAuthStore Credential = "auth_store"
	// CredentialBridgeServiceToken: llm-bridge-server's service token, sent
	// beside the caller's X-Principal-Id.
	CredentialBridgeServiceToken Credential = "bridge_service_token"
)

// Format tells a chip how to show a summary field's value. It is a hint about
// the value, not a rendering.
type Format string

const (
	FormatText             Format = "text"
	FormatNumber           Format = "number"
	FormatUnixSeconds      Format = "unix_seconds"
	FormatUnixMilliseconds Format = "unix_milliseconds"
	FormatISOTime          Format = "iso_time"
	FormatDate             Format = "date"
	FormatCents            Format = "cents"
	FormatProbability      Format = "probability"
	FormatURL              Format = "url"
	// FormatReference: the value is another record's id, drawn as its chip.
	FormatReference Format = "reference"
	FormatBoolean   Format = "boolean"
)

// Formats is every format, in the order GET /vocabulary serves them.
var Formats = []Format{FormatText, FormatNumber, FormatUnixSeconds, FormatUnixMilliseconds, FormatISOTime, FormatDate, FormatCents, FormatProbability, FormatURL, FormatReference, FormatBoolean}

// SummaryField is one value a chip shows under the record's name. Field is a
// dotted path into the record (messages.0.room_id).
type SummaryField struct {
	Field  string `json:"field"`
	Label  string `json:"label"`
	Format Format `json:"format"`
}

// ReferenceType is one row of the list.
type ReferenceType struct {
	Type    string `json:"type"`
	Service string `json:"service"`
	// Description says what a record of this type is, in a sentence.
	Description string `json:"description"`
	// Get is the owning service's fetch-one route, "{id}" marking the id.
	// Empty: ids of this type cannot be looked up, only linked.
	Get string `json:"get,omitempty"`
	// IDPatterns describe the whole id. Empty: never spotted in prose.
	IDPatterns []string `json:"id_patterns,omitempty"`
	// Search is the service's search route with the query appended;
	// SearchResults the key of its answer holding the records ("" when the
	// answer is the array). Empty Search: nothing to offer an @ picker.
	Search        string `json:"search,omitempty"`
	SearchResults string `json:"search_results,omitempty"`
	// LabelField is the dotted path of the field that names a record.
	LabelField string `json:"label_field,omitempty"`
	// Summary is what a chip shows under the name.
	Summary []SummaryField `json:"summary,omitempty"`
	// Page is the record's page: a path with {id} for the id asked about and
	// {data.<path>} for a field of the record. Empty: the record has no page
	// of its own and the chip opens the generic record page.
	Page string `json:"page,omitempty"`
	// PageSite names the site in site_urls the page is on; empty means the
	// site drawing the chip.
	PageSite string `json:"page_site,omitempty"`
	// Glyph is the chip's symbol.
	Glyph string `json:"glyph,omitempty"`
	// Attachment is the owning service's route for a picture of the record,
	// "{id}" marking the id: what a chat sends as an attached file beside the
	// record's link. Empty: the record has none.
	Attachment string     `json:"attachment,omitempty"`
	Access     Access     `json:"access"`
	Credential Credential `json:"credential,omitempty"`

	compiled []*regexp.Regexp
}

// Resolvable says whether an id of this type can be looked up.
func (t *ReferenceType) Resolvable() bool { return t.Get != "" && len(t.compiled) > 0 }

// Matches says whether id is shaped like this type's ids.
func (t *ReferenceType) Matches(id string) bool {
	for _, pattern := range t.compiled {
		if pattern.MatchString(id) {
			return true
		}
	}
	return false
}

// Mentionable says whether the @ picker may offer this type's records: it can
// find them, name them, and later resolve the id it inserts.
func (t *ReferenceType) Mentionable() bool {
	return t.Search != "" && t.LabelField != "" && t.Resolvable()
}

func text(field, label string) SummaryField { return SummaryField{field, label, FormatText} }
func seconds(field, label string) SummaryField {
	return SummaryField{field, label, FormatUnixSeconds}
}
func milliseconds(field, label string) SummaryField {
	return SummaryField{field, label, FormatUnixMilliseconds}
}
func reference(field, label string) SummaryField {
	return SummaryField{field, label, FormatReference}
}

// Types is the list, in the order GET /types serves it.
var Types = buildTypes()

func buildTypes() []ReferenceType {
	rows := []ReferenceType{
		{
			Type: "session", Service: "llm-bridge-server", Glyph: "💬",
			Description: "A chat session with an agent on this server.",
			Get:         "/sessions/{id}",
			// A bridge session id: a snowflake with br_, or a herald-/autoworker-
			// name ending in one. Harness session uuids are not listed:
			// llm-bridge-server cannot fetch by them.
			IDPatterns: []string{`br_\d{16,19}`, `(?:herald|autoworker)(?:-[a-z0-9]+)*-\d{16,19}`},
			LabelField: "display_name",
			Summary:    []SummaryField{text("state", "State"), text("harness", "Harness"), {"updated_at", "Last active", FormatISOTime}, {"spend_usd", "Spent (USD)", FormatNumber}},
			Page:       "/?session={id}",
			Access:     AccessByOwner, Credential: CredentialBridgeServiceToken,
		},
		{Type: "instance", Service: "llm-bridge-server", Search: "/api/instances?q=", Access: AccessByGrant, Description: "A harness instance sessions run on."},
		{Type: "machine", Service: "llm-bridge-server", Search: "/machines", Access: AccessByGrant, Description: "A machine harness instances run on."},
		{Type: "service", Service: "healthcheck", Search: "/api/services?q=", Access: AccessByGrant, Description: "A service healthcheck watches."},
		{Type: "skill", Service: "skill-store", Search: "/skills?q=", Access: AccessByGrant, Description: "A skill in skill-store; its ids are small integers, so it is never spotted in prose."},
		{Type: "tool", Service: "tool-store", Search: "/tools?q=", Access: AccessByGrant, Description: "A tool in tool-store; its ids are small integers, so it is never spotted in prose."},
		{
			// A card link's repo ref is a local path; prose names a repo as
			// repo:<name>, and only that form matches.
			Type: "repo", Service: "repo-store", Glyph: "📦",
			Description: "A git repository repo-store knows, written repo:<name>.",
			Get:         "/repos/by-mention/{id}", IDPatterns: []string{`repo:[\w.-]+`},
			LabelField: "name",
			Summary:    []SummaryField{{"github_url", "GitHub", FormatURL}},
			Page:       "/?view=work-graph&repo={data.id}",
			Access:     AccessByGrant,
		},
		{Type: "git_repo", Access: AccessByGrant, Description: "A git remote URL; nothing looks it up."},
		{
			// <repo>@<sha>: repo-store's name or the GitHub owner/name, and at
			// least seven hex characters of sha.
			Type: "commit", Service: "work-graph-store", Glyph: "🔖",
			Description: "A commit, written <repo>@<sha>.",
			Get:         "/commits/{id}", IDPatterns: []string{`(?:[\w.-]+/)?[\w.-]+@[0-9a-f]{7,40}`},
			LabelField: "commit.subject",
			Summary:    []SummaryField{text("repo.name", "Repo"), text("commit.author_name", "Author"), seconds("commit.committed_at", "Committed")},
			Page:       "/?view=work-graph&repo={data.repo.id}&commit={data.commit.sha}",
			Access:     AccessByGrant,
		},
		{Type: "agent", Service: "agent-store", Search: "/api/agents?q=", Access: AccessByGrant, Description: "An agent identity in agent-store."},
		{
			// /api/search, not /api/items: /api/items ignores q. One id space
			// for every noteboard item type; the item's own type says which.
			Type: "note", Service: "noteboard", Glyph: "📝",
			Description: "A noteboard item: a note, todo, ranked list or workspace.",
			Search:      "/api/search?q=", Get: "/api/items/{id}", IDPatterns: []string{UUIDPattern},
			LabelField: "title",
			Summary:    []SummaryField{text("type", "Kind"), text("status", "Status"), {"tags", "Tags", FormatText}},
			Page:       "/notes?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "prediction", Service: "prediction-store", Glyph: "🎲",
			Description: "A prediction with its odds and how it resolved.",
			Search:      "/predictions?q=", SearchResults: "predictions", Get: "/predictions/{id}", IDPatterns: []string{`prediction_\d{6,}`},
			LabelField: "claim",
			Summary:    []SummaryField{{"probability", "Odds", FormatProbability}, text("status", "Status"), text("outcome", "Outcome"), seconds("due_at", "Due")},
			Page:       "/predictions?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "article", Service: "article-store", Glyph: "📰",
			Description: "An article, essay or story saved from the web.",
			Search:      "/articles?q=", SearchResults: "articles", Get: "/articles/{id}", IDPatterns: []string{`article_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{text("byline", "By"), text("site_name", "Site"), {"word_count", "Words", FormatNumber}},
			Page:       "/articles?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "project", Service: "project-store", Glyph: "🧭",
			Description: "A project: a goal, its stage and when it is done.",
			Search:      "/projects?q=", SearchResults: "projects", Get: "/projects/{id}", IDPatterns: []string{`project_\d{6,}`},
			LabelField: "name",
			Summary:    []SummaryField{text("stage", "Stage"), text("goal", "Goal")},
			Page:       "/?view=projects&project={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "principal", Service: "principal-store", Glyph: "🔑",
			Description: "Someone or something that signs in, holds grants or is assigned work.",
			Search:      "/principals?q=", Get: "/principals/{id}", IDPatterns: []string{PrincipalIDPattern},
			LabelField: "display_name",
			Summary:    []SummaryField{text("kind", "Kind"), text("email", "Email"), {"is_administrator", "Administrator", FormatBoolean}},
			Page:       "/principals?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "person", Service: "people-store", Glyph: "👤",
			Description: "A person in the operator's life.",
			Search:      "/persons?q=", Get: "/persons/{id}", IDPatterns: []string{`person_\d{6,}`},
			LabelField: "display_name",
			Summary:    []SummaryField{text("how_met", "How met"), {"birthday", "Birthday", FormatDate}},
			Page:       "/people?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "entry", Service: "journal-store", Glyph: "📓",
			Description: "A piece of the operator's own writing: a journal entry, post or note.",
			Search:      "/entries?q=", Get: "/entries/{id}", IDPatterns: []string{`entry_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{text("kind", "Kind"), seconds("written_at", "Written")},
			Page:       "/journal?id={id}",
			Access:     AccessByGrant,
		},
		{
			// Get answers a page holding the one message; its fields are under
			// messages.0.
			Type: "message", Service: "multichat", Glyph: "✉️",
			Description: "One message in multichat's log: a text, a Discord or WhatsApp message.",
			Get:         "/api/message-log/{id}", IDPatterns: []string{`message_\d{6,}`},
			LabelField: "messages.0.body",
			Summary:    []SummaryField{text("messages.0.sender_display_name", "From"), text("messages.0.room_name", "Room"), text("messages.0.platform", "Platform")},
			Page:       "/messages?room={data.messages.0.room_id}",
			Access:     AccessByGrant, Credential: CredentialAuthStore,
		},
		{
			Type: "photo", Service: "photo-store", Glyph: "🖼️",
			Description: "One of the operator's photos.",
			Get:         "/photos/{id}", IDPatterns: []string{`photo_\d{6,}`},
			LabelField: "description",
			Summary:    []SummaryField{seconds("taken_at", "Taken"), text("folder", "Folder")},
			Page:       "/photos?id={id}",
			Attachment: "/photos/{id}/thumbnail",
			Access:     AccessByGrant,
		},
		{
			Type: "scheduler_job", Service: "scheduler", Glyph: "⏰",
			Description: "A scheduled job: a shell command or an agent prompt run on a cron schedule or on bus events.",
			// The scheduler numbers jobs 1, 2, 3; scheduler_job_000076 is how
			// text names job 76, and the route takes that form.
			Get: "/api/job-references/{id}", IDPatterns: []string{`scheduler_job_\d{6,}`},
			LabelField: "name",
			Summary: []SummaryField{text("schedule", "Schedule"), text("enabled", "Enabled"),
				text("last_run.status", "Last run"), {"last_run.finished_at", "Finished", FormatISOTime}},
			Page:   "/scheduler?id={data.id}",
			Access: AccessByGrant,
		},
		{
			Type: "face_group", Service: "photo-store", Glyph: "🙂",
			Description: "Faces the face model found to be one person, with who they are once named.",
			Get:         "/face-groups/{id}", IDPatterns: []string{`face_group_\d{6,}`},
			// A group has no name of its own: the person it names is a summary
			// field, drawn as that person's chip.
			Summary: []SummaryField{reference("person_id", "Person"), text("status", "Status"),
				{"photo_count", "Photos", FormatNumber}},
			Page:   "/faces?id={id}",
			Access: AccessByGrant,
		},
		{
			Type: "place", Service: "location-store", Glyph: "📍",
			Description: "A named place.",
			Get:         "/places/{id}", IDPatterns: []string{`place_\d{6,}`},
			LabelField: "name",
			Summary:    []SummaryField{text("address", "Address"), text("kind", "Kind")},
			Page:       "/places?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "visit", Service: "location-store", Glyph: "📍",
			Description: "A stay at a place.",
			Get:         "/visits/{id}", IDPatterns: []string{`visit_\d{6,}`},
			LabelField: "place_name",
			Summary:    []SummaryField{seconds("started_at", "From"), seconds("ended_at", "To"), reference("place_id", "Place")},
			Page:       "/places?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "trip", Service: "location-store", Glyph: "🧳",
			Description: "A movement between two places, or a trip grouping them.",
			Get:         "/trips/{id}", IDPatterns: []string{`trip_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{text("mode", "By"), seconds("started_at", "From"), seconds("ended_at", "To"), {"distance_meters", "Meters", FormatNumber}},
			Page:       "/places?id={id}",
			Access:     AccessByGrant,
		},
		{
			Type: "conversation", Service: "message-store", Glyph: "🗨️",
			Description: "One chat with one person or group, on one platform.",
			Get:         "/conversations/{id}", IDPatterns: []string{`conversation_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{text("platform", "Platform"), {"message_count", "Messages", FormatNumber}, milliseconds("last_message_at", "Last message")},
			Access:     AccessByGrant,
		},
		{
			Type: "chat_message", Service: "message-store", Glyph: "🗨️",
			Description: "One chat message the operator sent or received.",
			Get:         "/messages/{id}", IDPatterns: []string{`chat_message_\d{6,}`},
			LabelField: "body",
			Summary:    []SummaryField{text("sender_name", "From"), milliseconds("sent_at", "Sent"), reference("conversation_id", "Conversation")},
			Access:     AccessByGrant,
		},
		{
			Type: "conversation_day", Service: "message-store", Glyph: "🗓️",
			Description: "One chat on one local day.",
			Get:         "/conversation-days/{id}", IDPatterns: []string{`conversation_day_\d{6,}`},
			LabelField: "day",
			Summary:    []SummaryField{{"message_count", "Messages", FormatNumber}, reference("conversation_id", "Conversation")},
			Page:       "/day/{data.day}?conversation={data.conversation_id}#{id}",
			Access:     AccessByGrant,
		},
		{
			Type: "discord_event", Service: "discord-signup-store", Glyph: "🎟️",
			Description: "An event organized on Discord, with who signed up.",
			Get:         "/api/discord-events/{id}", IDPatterns: []string{`discord_event_\d{6,}`},
			LabelField: "name",
			Summary:    []SummaryField{seconds("starts_at", "Starts"), text("status", "Status"), {"capacity", "Capacity", FormatNumber}},
			Access:     AccessByGrant,
		},
		{
			Type: "discord_attendance", Service: "discord-signup-store", Glyph: "🎟️",
			Description: "One person who went to one date of a Discord event.",
			Get:         "/api/discord-attendance/{id}", IDPatterns: []string{`discord_attendance_\d{6,}`},
			LabelField: "display_name",
			Summary:    []SummaryField{reference("event_ref", "Event"), seconds("occurrence_starts_at", "Date"), text("evidence", "Evidence")},
			Access:     AccessByGrant,
		},
		{
			Type: "calendar_event", Service: "calendar-store", Glyph: "📅",
			Description: "A calendar event as its calendar wrote it.",
			Get:         "/events/{id}", IDPatterns: []string{`calendar_event_\d{6,}`},
			LabelField: "summary",
			Summary:    []SummaryField{seconds("starts_at", "Starts"), text("location", "Where")},
			Access:     AccessByGrant,
		},
		{
			Type: "time_entry", Service: "time-ledger-store", Glyph: "⏱️",
			Description: "One thing the operator did, or planned and skipped, over a span of time.",
			Get:         "/time-entries/{id}", IDPatterns: []string{`time_entry_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{seconds("starts_at", "Starts"), text("outcome", "Outcome"), text("place", "Where"), reference("parent_time_entry_id", "Inside")},
			Access:     AccessByGrant,
		},
		{
			Type: "time_entry_kind", Service: "time-ledger-store", Glyph: "🏷️",
			Description: "A kind of thing a time entry can be; an entry can be several.",
			Get:         "/kinds/{id}", IDPatterns: []string{`time_entry_kind_\d{6,}`},
			LabelField: "name",
			Summary:    []SummaryField{text("description", "Means")},
			Access:     AccessByGrant,
		},
		{
			Type: "time_entry_review", Service: "time-ledger-store", Glyph: "⚖️",
			Description: "A judgment, by a model or the operator, of whether a time entry happened.",
			Get:         "/reviews/{id}", IDPatterns: []string{`time_entry_review_\d{6,}`},
			LabelField: "outcome",
			Summary:    []SummaryField{reference("time_entry_id", "Entry"), text("reviewed_by", "By"), text("reason", "Why")},
			Access:     AccessByGrant,
		},
		{
			Type: "activity", Service: "activity-store", Glyph: "🔎",
			Description: "One thing done online: a search, page visit, video or app.",
			Get:         "/activities/{id}", IDPatterns: []string{`activity_\d{6,}`},
			LabelField: "title",
			Summary:    []SummaryField{text("action_text", "Did"), text("product", "Where"), seconds("occurred_at", "When"), {"url", "Link", FormatURL}},
			Access:     AccessByGrant,
		},
		// Email: no pattern and no Get. A card links one by a compound ref
		// mailstack resolves on its own routes.
		{Type: "email", Service: "mailstack", Access: AccessByGrant, Description: "An email, as <account_id>:<provider message id>."},
		{Type: "email_msgid", Service: "mailstack", Access: AccessByGrant, Description: "An email's RFC 5322 Message-ID."},
		{Type: "email_sender", Service: "mailstack", Access: AccessByGrant, Description: "A sender's lowercased address."},
	}
	for _, table := range []string{
		"instrument", "tuning", "technique", "fretboard_shape", "interval", "scale_degree",
		"chord_type", "scale_type", "progression", "rhythm_pattern", "form", "expression_mark",
		"theory_concept", "ability", "style", "figure", "song", "performance", "arrangement",
		"passage", "lesson", "exercise", "drill", "curriculum", "curriculum_level", "requirement", "goal",
	} {
		rows = append(rows, musicPracticeType(table))
	}
	rows = append(rows,
		budgetType("budget_account", "account", "/accounts/{id}", "name", "An account: checking, card, loan or retirement.",
			[]SummaryField{text("kind", "Kind"), {"balance_cents", "Balance", FormatCents}, text("last_four", "Ends in")},
			"/budget?tab=transactions&account={id}"),
		budgetType("budget_transaction", "transaction", "/transactions/{id}", "description", "A transaction in the books.",
			[]SummaryField{{"occurred_on", "On", FormatDate}, text("status", "Status"), reference("payee_id", "Payee")}, ""),
		budgetType("budget_category", "category", "/categories/{id}", "name", "A spending category.",
			[]SummaryField{text("kind", "Kind"), reference("parent_id", "Under")},
			"/budget?tab=transactions&category={id}"),
		budgetType("budget_payee", "payee", "/payees/{id}", "name", "Someone money goes to or comes from.",
			[]SummaryField{reference("default_category_id", "Usually")}, ""),
		budgetType("budget_marketplace_order", "marketplace_order", "/marketplace-orders/{id}", "seller", "An order placed on a marketplace (eBay), what it bought and the charge that paid it.",
			[]SummaryField{text("order_number", "Order"), seconds("ordered_at", "Ordered"), {"total_cents", "Total", FormatCents}, reference("transaction_id", "Charge")}, ""),
		budgetType("budget_marketplace_order_line", "marketplace_order_line", "/marketplace-order-lines/{id}", "title", "One thing a marketplace order bought.",
			[]SummaryField{reference("order_id", "Order"), {"listing_url", "Listing", FormatURL}, text("disposition", "Became")}, ""),
		budgetType("budget_import_batch", "import_batch", "/import-batches/{id}", "file_name", "One bank statement imported.",
			[]SummaryField{reference("account_id", "Account"), {"first_date", "From", FormatDate}, {"last_date", "To", FormatDate}}, ""),
	)
	// A kind record's Get answers the whole possession, whose own id is
	// possession_…: the page takes that one.
	possession := []SummaryField{text("kind", "Kind"), text("brand", "Brand"), text("status", "Status"), reference("cover_photo_id", "Photo")}
	for _, kind := range []struct{ typeName, get, description string }{
		{"possession", "/possessions/{id}", "Something the operator owns."},
		{"garment", "/garments/{id}", "A garment the operator owns."},
		{"computer", "/computers/{id}", "A computer the operator owns."},
		{"computer_part", "/computer-parts/{id}", "A computer part the operator owns."},
		{"audio_device", "/audio-devices/{id}", "An audio device the operator owns."},
		{"musical_instrument", "/musical-instruments/{id}", "A musical instrument the operator owns."},
		{"vehicle", "/vehicles/{id}", "A vehicle the operator owns."},
	} {
		row := possessionType(kind.typeName, kind.get, kind.description, "name", possession, "/possessions?id={data.id}")
		row.Attachment = kind.get + "/image"
		rows = append(rows, row)
	}
	rows = append(rows,
		withAttachment(possessionType("outfit", "/outfits/{id}", "Garments worn together.", "name",
			[]SummaryField{text("occasion", "Occasion"), text("status", "Status")}, "/outfits?id={id}"), "/outfits/{id}/image"),
		possessionType("possession_purchase", "/purchases/{id}", "A purchase of possessions.", "seller",
			[]SummaryField{text("marketplace", "Where"), seconds("ordered_at", "Ordered"), {"total_cents", "Total", FormatCents}}, ""),
		possessionType("product", "/products/{id}", "A make and model, as its maker's listing describes it.", "name",
			[]SummaryField{text("brand", "Brand"), text("model_number", "Model"), {"listing_url", "Listing", FormatURL}}, ""),
		possessionType("photo_review", "/photo-reviews/{id}", "Two models' reading of one photo for possessions.", "",
			[]SummaryField{reference("photo_id", "Photo"), text("reading", "Reading"), seconds("finished_at", "Finished")}, ""),
		possessionType("sighting", "/sightings/{id}", "Something seen on a photo that may be a possession.", "label",
			[]SummaryField{text("status", "Status"), reference("possession_id", "Possession"), reference("photo_id", "Photo")}, ""),
		possessionType("possession_question", "/owner-questions/{id}", "A question for the owner about something on a photo.", "question",
			[]SummaryField{text("answer_text", "Answer"), reference("photo_id", "Photo")}, ""),
	)
	for i := range rows {
		for _, pattern := range rows[i].IDPatterns {
			rows[i].compiled = append(rows[i].compiled, regexp.MustCompile(`(?i)^(?:`+pattern+`)$`))
		}
	}
	return rows
}

func budgetType(typeName, idPrefix, get, labelField, description string, summary []SummaryField, page string) ReferenceType {
	return ReferenceType{
		Type: typeName, Service: "budget-store", Glyph: "💵", Description: description,
		Get: get, IDPatterns: []string{idPrefix + `_\d{6,}`}, LabelField: labelField,
		Summary: summary, Page: page, Access: AccessByGrant,
	}
}

// withAttachment gives a row the route that serves a picture of its record.
func withAttachment(row ReferenceType, attachment string) ReferenceType {
	row.Attachment = attachment
	return row
}

func possessionType(typeName, get, description, labelField string, summary []SummaryField, page string) ReferenceType {
	return ReferenceType{
		Type: typeName, Service: "possession-store", Glyph: "🧺", Description: description,
		Get: get, IDPatterns: []string{typeName + `_\d{6,}`}, LabelField: labelField,
		Summary: summary, Page: page, Access: AccessByGrant,
	}
}

// musicPracticeType is one music-practice-store record type. Every record has a
// page on the music site; a drill's page runs it, and a curriculum level's
// lists what it asks.
func musicPracticeType(table string) ReferenceType {
	prefix := "music_" + table
	page := "/music/records/{id}"
	switch table {
	case "drill":
		page = "/music/drills/{id}"
	case "curriculum_level":
		page = "/music/curricula/levels/{id}"
	}
	return ReferenceType{
		Type: prefix, Service: "music-practice-store", Glyph: "🎵",
		Description: "A " + strings.ReplaceAll(table, "_", " ") + " in music practice.",
		Search:      "/api/music-practice/types/" + table + "/records?q=", SearchResults: "records",
		Get: "/api/music-practice/records/{id}", IDPatterns: []string{prefix + `_\d{6,}`},
		LabelField: "label", Summary: []SummaryField{text("description", "")},
		Page: page, PageSite: "music",
		Access: AccessByGrant, Credential: CredentialAuthStore,
	}
}

// TypeNamed returns the row for a type, or nil.
func TypeNamed(name string) *ReferenceType {
	for i := range Types {
		if Types[i].Type == name {
			return &Types[i]
		}
	}
	return nil
}

// ValidateReferenceConfiguration checks the catalog and supplied addresses.
// Services and page sites may be unconfigured on this deployment.
func ValidateReferenceConfiguration(types []ReferenceType, serviceURLs, siteURLs map[string]string) error {
	seen := map[string]bool{}
	formats := map[Format]bool{}
	for _, format := range Formats {
		formats[format] = true
	}
	var problems []string
	for _, row := range types {
		if seen[row.Type] {
			problems = append(problems, fmt.Sprintf("type %q is declared twice", row.Type))
		}
		seen[row.Type] = true
		if row.Access != AccessByGrant && row.Access != AccessByOwner {
			problems = append(problems, fmt.Sprintf("type %q has access %q", row.Type, row.Access))
		}
		for _, field := range row.Summary {
			if !formats[field.Format] {
				problems = append(problems, fmt.Sprintf("type %q summary field %q has format %q", row.Type, field.Field, field.Format))
			}
		}
	}
	for kind, addresses := range map[string]map[string]string{"service": serviceURLs, "site": siteURLs} {
		for name, address := range addresses {
			if address == "" {
				continue
			}
			parsed, err := url.Parse(address)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				problems = append(problems, fmt.Sprintf("%s %q needs an absolute HTTP or HTTPS address", kind, name))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("reference types: %s", strings.Join(problems, "; "))
	}
	return nil
}

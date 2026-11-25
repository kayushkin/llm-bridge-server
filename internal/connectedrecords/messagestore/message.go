package messagestore

import "encoding/json"

// Wire types. generate-ts.sh renders this file and vocabulary.go to
// ts/model.ts; a type another program reads lives here or it is not rendered.

// Conversation is one chat on one platform.
type Conversation struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
	Title     string `json:"title"`
	IsGroup   bool   `json:"is_group"`
	// ParentSourceRef is, for a thread, the id of the channel it hangs off,
	// in the same source; empty for anything else.
	ParentSourceRef string `json:"parent_source_ref"`
	// Participants are everyone the store has seen in it, the user included.
	Participants []Participant `json:"participants"`
	// MessageCount, FirstMessageAt and LastMessageAt summarize its messages.
	MessageCount   int   `json:"message_count"`
	FirstMessageAt int64 `json:"first_message_at"`
	LastMessageAt  int64 `json:"last_message_at"`
	CreatedAt      int64 `json:"created_at"`
	UpdatedAt      int64 `json:"updated_at"`
}

// Participant is one handle in a conversation and the person it belongs to.
type Participant struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	// PersonID is people-store's id, or empty when nobody is known.
	PersonID string `json:"person_id"`
	IsSelf   bool   `json:"is_self"`
}

// Attachment is a picture, video or file sent with a message.
type Attachment struct {
	// PhotoID is photo-store's id when photo-store keeps the picture.
	PhotoID  string `json:"photo_id,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Embed is a card a bot posts beside or instead of text: Discord's embed,
// with the parts a reader sees.
type Embed struct {
	Title        string       `json:"title,omitempty"`
	Description  string       `json:"description,omitempty"`
	URL          string       `json:"url,omitempty"`
	AuthorName   string       `json:"author_name,omitempty"`
	Fields       []EmbedField `json:"fields,omitempty"`
	FooterText   string       `json:"footer_text,omitempty"`
	ImageURL     string       `json:"image_url,omitempty"`
	ThumbnailURL string       `json:"thumbnail_url,omitempty"`
}

// EmbedField is one named value in an embed.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Button is a button under a message, as the reader sees it. URL is set on a
// link button; a button that runs something has none.
type Button struct {
	Label    string `json:"label"`
	Emoji    string `json:"emoji,omitempty"`
	URL      string `json:"url,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Account is an account of ours on a platform: the user's own, or a bot the
// user runs. Its conversations are those with a participant of its handle on
// its platform.
type Account struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	// Kind is one of AccountKinds.
	Kind string `json:"kind"`
	// CredentialID is auth-store's id of the credential that signs in as
	// the account (a bot's token), or empty when none is kept.
	CredentialID string `json:"credential_id"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// AccountPut is POST /accounts: an account found by (platform, handle),
// made if new. An empty display name keeps the stored one.
type AccountPut struct {
	Platform    string `json:"platform"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	// CredentialID, when set, replaces the stored one.
	CredentialID string `json:"credential_id"`
}

// Message is one message, as the store answers it.
type Message struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	// SentAt is unix milliseconds, UTC.
	SentAt       int64  `json:"sent_at"`
	SenderHandle string `json:"sender_handle"`
	// SenderName, SenderPersonID and FromSelf are read through the
	// conversation's participant row for SenderHandle.
	SenderName     string `json:"sender_name"`
	SenderPersonID string `json:"sender_person_id"`
	FromSelf       bool   `json:"from_self"`
	Kind           string `json:"kind"`
	Body           string `json:"body"`
	// BodySource is one of BodySources: whose text Body is.
	BodySource        string       `json:"body_source"`
	Attachments       []Attachment `json:"attachments"`
	Embeds            []Embed      `json:"embeds"`
	Buttons           []Button     `json:"buttons"`
	ReplyToSourceRef  string       `json:"reply_to_source_ref,omitempty"`
	CallSeconds       int          `json:"call_seconds,omitempty"`
	Edited            bool         `json:"edited,omitempty"`
	DeletedOnPlatform bool         `json:"deleted_on_platform,omitempty"`
	Source            string       `json:"source"`
	SourceRef         string       `json:"source_ref"`
	ImportedBy        string       `json:"imported_by"`
}

// ConversationDay is one conversation on one local day.
type ConversationDay struct {
	ID               string `json:"id"`
	ConversationID   string `json:"conversation_id"`
	Day              string `json:"day"`
	TimeZone         string `json:"time_zone"`
	StartsAt         int64  `json:"starts_at"`
	EndsAt           int64  `json:"ends_at"`
	JournalSource    string `json:"journal_source"`
	JournalSourceRef string `json:"journal_source_ref"`
	JournalEntryID   string `json:"journal_entry_id"`
	// MessageCount counts the conversation's messages inside the span.
	MessageCount int `json:"message_count"`
	// Involvement is one of DayInvolvements: how the user took part that day.
	Involvement string `json:"involvement"`
}

// ConversationDayWithMessages is GET /conversation-days/{id}: the day, its
// conversation and its messages in time order.
type ConversationDayWithMessages struct {
	ConversationDay `tstype:",extends"`
	Conversation    Conversation `json:"conversation"`
	Messages        []Message    `json:"messages"`
}

// MessagePage is a list of messages and the cursor for the next page.
type MessagePage struct {
	Messages []Message `json:"messages"`
	// Next is the cursor to pass as `cursor` for the following page, or empty
	// at the end.
	Next string `json:"next"`
}

// ActivityBucket counts messages in one span of time for one key (a person, a
// conversation or a platform, as the request groups them).
type ActivityBucket struct {
	// Start is the bucket's first instant, unix ms, in the time zone asked.
	Start    int64  `json:"start"`
	Key      string `json:"key"`
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
}

// ImportStart is POST /imports.
type ImportStart struct {
	Importer    string `json:"importer"`
	Description string `json:"description"`
}

// Import is one run of an importer.
type Import struct {
	ID          string       `json:"id"`
	Importer    string       `json:"importer"`
	Description string       `json:"description"`
	StartedAt   int64        `json:"started_at"`
	FinishedAt  int64        `json:"finished_at"`
	Counts      ImportCounts `json:"counts"`
}

// ImportBatch is POST /imports/{id}/batches. Conversations come before the
// messages and days that name them, in this batch or an earlier one. One bad
// record writes nothing of the batch.
type ImportBatch struct {
	Conversations    []ImportConversation    `json:"conversations"`
	Messages         []ImportMessage         `json:"messages"`
	ConversationDays []ImportConversationDay `json:"conversation_days"`
	// Supersessions retire rows in favor of messages already stored,
	// written after the batch's messages.
	Supersessions []ImportSupersession `json:"supersessions,omitempty"`
	// ConversationMerges merge conversations that are one chat, written
	// after the supersessions and before the days.
	ConversationMerges []ImportConversationMerge `json:"conversation_merges,omitempty"`
}

// ConversationKey finds a conversation by its id in the namespace it came from.
type ConversationKey struct {
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
}

// ImportConversationMerge merges the conversation filed under Merged into the
// one filed under Into, for Rule (one of ConversationMergeRules). Either key
// may name a conversation merged earlier, which stands for the one it merged
// into. A side the store does not hold is skipped and counted; two that are
// already one is nothing to do.
type ImportConversationMerge struct {
	Merged ConversationKey `json:"merged"`
	Into   ConversationKey `json:"into"`
	Rule   string          `json:"rule"`
}

// ConversationMerge is a conversation merged into another. Row is its row as
// it was, with its participants.
type ConversationMerge struct {
	ConversationID string          `json:"conversation_id"`
	Source         string          `json:"source"`
	SourceRef      string          `json:"source_ref"`
	MergedInto     string          `json:"merged_into"`
	Rule           string          `json:"rule"`
	ImportID       string          `json:"import_id"`
	MergedAt       int64           `json:"merged_at"`
	MessagesMoved  int             `json:"messages_moved"`
	Row            json.RawMessage `json:"row" tstype:"{ [column: string]: unknown }"`
}

// ImportSupersession retires the row filed under Superseded in favor of the
// stored message filed under By, as an ImportMessage's supersedes does, for
// an importer that does not own By's row. A Superseded row that is not
// stored, or already retired, is nothing to do; a By that is not stored is a
// 400.
type ImportSupersession struct {
	Superseded MessageKey `json:"superseded"`
	By         MessageKey `json:"by"`
}

// ImportConversation is a conversation found by (source, source_ref).
// Participants are added or updated by handle; one the batch leaves out is
// kept. A participant's person_id is written only when the batch gives one,
// so a person set by hand survives an import that knows nobody.
type ImportConversation struct {
	Platform  string `json:"platform"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
	Title     string `json:"title"`
	IsGroup   bool   `json:"is_group"`
	// ParentSourceRef names a thread's channel; empty keeps the stored one.
	ParentSourceRef string        `json:"parent_source_ref,omitempty"`
	Participants    []Participant `json:"participants"`
}

// ImportMessage is a message found by (source, source_ref). Its conversation
// is named by the conversation's own source and source_ref.
type ImportMessage struct {
	ConversationSource    string `json:"conversation_source"`
	ConversationSourceRef string `json:"conversation_source_ref"`
	SentAt                int64  `json:"sent_at"`
	SenderHandle          string `json:"sender_handle"`
	Kind                  string `json:"kind"`
	Body                  string `json:"body"`
	// BodySource is one of BodySources, and required: a bridge's text never
	// replaces the platform's own.
	BodySource  string       `json:"body_source"`
	Attachments []Attachment `json:"attachments"`
	// Embeds and Buttons replace the stored ones when the import sends any;
	// none keeps what is stored.
	Embeds            []Embed  `json:"embeds,omitempty"`
	Buttons           []Button `json:"buttons,omitempty"`
	ReplyToSourceRef  string   `json:"reply_to_source_ref"`
	CallSeconds       int      `json:"call_seconds"`
	Edited            bool     `json:"edited"`
	DeletedOnPlatform bool     `json:"deleted_on_platform"`
	Source            string   `json:"source"`
	SourceRef         string   `json:"source_ref"`
	// SourceHash is the importer's hash of what it read; an unchanged hash
	// skips the row.
	SourceHash string `json:"source_hash"`
	// Supersedes names rows that hold this same message under another id
	// namespace. Each that exists is copied whole to the superseded record
	// and removed, and its id then redirects here. Sending a superseded
	// (source, source_ref) again is skipped and counted.
	Supersedes []MessageKey `json:"supersedes,omitempty"`
}

// MessageKey names a message by its id namespace and its id there.
type MessageKey struct {
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
}

// SupersededMessage is a message row retired because another row holds the
// same message. Row is every column of the retired row, as it was.
type SupersededMessage struct {
	ID           string          `json:"id"`
	Source       string          `json:"source"`
	SourceRef    string          `json:"source_ref"`
	SupersededBy string          `json:"superseded_by"`
	ImportID     string          `json:"import_id"`
	SupersededAt int64           `json:"superseded_at"`
	Row          json.RawMessage `json:"row" tstype:"{ [column: string]: unknown }"`
}

// ImportConversationDay names one local day of a conversation, and the
// journal-store transcript that held it, if one did.
type ImportConversationDay struct {
	ConversationSource    string `json:"conversation_source"`
	ConversationSourceRef string `json:"conversation_source_ref"`
	Day                   string `json:"day"`
	TimeZone              string `json:"time_zone"`
	JournalSource         string `json:"journal_source"`
	JournalSourceRef      string `json:"journal_source_ref"`
}

// ImportCounts is what a batch, or a whole import, did.
type ImportCounts struct {
	ConversationsCreated int `json:"conversations_created"`
	ConversationsUpdated int `json:"conversations_updated"`
	MessagesCreated      int `json:"messages_created"`
	MessagesUpdated      int `json:"messages_updated"`
	MessagesUnchanged    int `json:"messages_unchanged"`
	DaysCreated          int `json:"days_created"`
	DaysUpdated          int `json:"days_updated"`
	MessagesSuperseded   int `json:"messages_superseded"`
	// MessagesSupersededSkipped counts messages sent under a key already
	// superseded: skipped, since another row holds the message.
	MessagesSupersededSkipped int `json:"messages_superseded_skipped"`
	ConversationsMerged       int `json:"conversations_merged"`
	// ConversationMergesSkipped counts merges naming a conversation the
	// store does not hold.
	ConversationMergesSkipped int `json:"conversation_merges_skipped"`
}

// JournalEntryLink is one item of POST /conversation-days/journal-entries: the
// journal-store entry that holds the transcript with this source and
// source_ref.
type JournalEntryLink struct {
	JournalSource    string `json:"journal_source"`
	JournalSourceRef string `json:"journal_source_ref"`
	JournalEntryID   string `json:"journal_entry_id"`
}

// JournalEntryLinkResult says how many links matched a day and which did not.
type JournalEntryLinkResult struct {
	Linked    int                `json:"linked"`
	Unmatched []JournalEntryLink `json:"unmatched"`
}

// ParticipantPatch is PATCH /conversations/{id}/participants/{handle}.
type ParticipantPatch struct {
	PersonID *string `json:"person_id"`
}

// Summary is GET /summary: how much the store holds, by platform.
type Summary struct {
	Platforms []PlatformSummary `json:"platforms"`
}

// PlatformSummary counts one platform's conversations and messages.
type PlatformSummary struct {
	Platform       string `json:"platform"`
	Conversations  int    `json:"conversations"`
	Messages       int    `json:"messages"`
	FirstMessageAt int64  `json:"first_message_at"`
	LastMessageAt  int64  `json:"last_message_at"`
}

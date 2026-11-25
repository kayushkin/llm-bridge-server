package messagestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MessageFilter narrows GET /messages. Zero values filter nothing.
type MessageFilter struct {
	ConversationIDs []string
	// PersonID keeps messages in conversations the person takes part in.
	PersonID string
	Platform string
	// Source and SourceRef keep the messages one importer filed under that
	// source and its own ref (a Discord message id); SourceRef needs Source.
	Source    string
	SourceRef string
	After     int64 // unix ms, inclusive
	Before    int64 // unix ms, exclusive
	Query     string
	// Newest lists newest first; the default is oldest first.
	Newest bool
	Limit  int
	Cursor string
}

const messageColumns = `m.id, m.conversation_id, m.sent_at, m.sender_handle, COALESCE(p.display_name, ''), COALESCE(p.person_id, ''), COALESCE(p.is_self, 0),
	m.kind, m.body, m.body_source, m.attachments, m.embeds, m.buttons, m.reply_to_source_ref, m.call_seconds, m.edited, m.deleted_on_platform, m.source, m.source_ref, m.imported_by, m.seq`

const messageJoin = `FROM messages m LEFT JOIN participants p ON p.conversation_id = m.conversation_id AND p.handle = m.sender_handle`

func scanMessage(rows interface{ Scan(...any) error }) (Message, int64, error) {
	var message Message
	var isSelf, edited, deleted int
	var attachments, embeds, buttons string
	var seq int64
	if err := rows.Scan(&message.ID, &message.ConversationID, &message.SentAt, &message.SenderHandle, &message.SenderName, &message.SenderPersonID, &isSelf,
		&message.Kind, &message.Body, &message.BodySource, &attachments, &embeds, &buttons, &message.ReplyToSourceRef, &message.CallSeconds, &edited, &deleted,
		&message.Source, &message.SourceRef, &message.ImportedBy, &seq); err != nil {
		return Message{}, 0, err
	}
	message.FromSelf, message.Edited, message.DeletedOnPlatform = isSelf == 1, edited == 1, deleted == 1
	if err := json.Unmarshal([]byte(attachments), &message.Attachments); err != nil {
		return Message{}, 0, fmt.Errorf("message %s attachments: %w", message.ID, err)
	}
	if err := json.Unmarshal([]byte(embeds), &message.Embeds); err != nil {
		return Message{}, 0, fmt.Errorf("message %s embeds: %w", message.ID, err)
	}
	if err := json.Unmarshal([]byte(buttons), &message.Buttons); err != nil {
		return Message{}, 0, fmt.Errorf("message %s buttons: %w", message.ID, err)
	}
	return message, seq, nil
}

// ListMessages answers GET /messages: messages in time order, paged by a
// cursor of the last row's time and seq.
func (s *Store) ListMessages(filter MessageFilter) (MessagePage, error) {
	if filter.Limit <= 0 || filter.Limit > 5000 {
		filter.Limit = 500
	}
	var where []string
	var args []any
	if len(filter.ConversationIDs) > 0 {
		live, err := s.liveConversationIDs(filter.ConversationIDs)
		if err != nil {
			return MessagePage{}, err
		}
		filter.ConversationIDs = live
		where = append(where, "m.conversation_id IN ("+placeholders(len(filter.ConversationIDs))+")")
		for _, id := range filter.ConversationIDs {
			args = append(args, id)
		}
	}
	if filter.PersonID != "" {
		where = append(where, "m.conversation_id IN (SELECT conversation_id FROM participants WHERE person_id = ?)")
		args = append(args, filter.PersonID)
	}
	if filter.Platform != "" {
		where = append(where, "m.conversation_id IN (SELECT id FROM conversations WHERE platform = ?)")
		args = append(args, filter.Platform)
	}
	if filter.SourceRef != "" && filter.Source == "" {
		return MessagePage{}, fmt.Errorf("%w: source_ref needs source, since refs are only unique within one", ErrInvalid)
	}
	if filter.Source != "" {
		where = append(where, "m.source = ?")
		args = append(args, filter.Source)
	}
	if filter.SourceRef != "" {
		where = append(where, "m.source_ref = ?")
		args = append(args, filter.SourceRef)
	}
	if filter.After > 0 {
		where = append(where, "m.sent_at >= ?")
		args = append(args, filter.After)
	}
	if filter.Before > 0 {
		where = append(where, "m.sent_at < ?")
		args = append(args, filter.Before)
	}
	if filter.Query != "" {
		where = append(where, "m.seq IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
		args = append(args, ftsQuery(filter.Query))
	}
	if filter.Cursor != "" {
		sentAt, seq, err := parseCursor(filter.Cursor)
		if err != nil {
			return MessagePage{}, err
		}
		if filter.Newest {
			where = append(where, "(m.sent_at < ? OR (m.sent_at = ? AND m.seq < ?))")
		} else {
			where = append(where, "(m.sent_at > ? OR (m.sent_at = ? AND m.seq > ?))")
		}
		args = append(args, sentAt, sentAt, seq)
	}
	order := "m.sent_at ASC, m.seq ASC"
	if filter.Newest {
		order = "m.sent_at DESC, m.seq DESC"
	}
	query := "SELECT " + messageColumns + " " + messageJoin
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + order + " LIMIT ?"
	args = append(args, filter.Limit+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return MessagePage{}, err
	}
	defer rows.Close()
	page := MessagePage{Messages: []Message{}}
	var lastSeq int64
	for rows.Next() {
		message, seq, err := scanMessage(rows)
		if err != nil {
			return MessagePage{}, err
		}
		if len(page.Messages) == filter.Limit {
			last := page.Messages[len(page.Messages)-1]
			page.Next = fmt.Sprintf("%d:%d", last.SentAt, lastSeq)
			break
		}
		page.Messages = append(page.Messages, message)
		lastSeq = seq
	}
	return page, rows.Err()
}

// ftsQuery quotes each word so punctuation in a search never reads as FTS
// syntax.
func ftsQuery(query string) string {
	var terms []string
	for _, word := range strings.Fields(query) {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " ")
}

func parseCursor(cursor string) (int64, int64, error) {
	sentAtText, seqText, found := strings.Cut(cursor, ":")
	sentAt, err1 := strconv.ParseInt(sentAtText, 10, 64)
	seq, err2 := strconv.ParseInt(seqText, 10, 64)
	if !found || err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("%w: cursor %q", ErrInvalid, cursor)
	}
	return sentAt, seq, nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// GetMessage reads one message.
// MessageRevision is a message as it was before an import changed it.
type MessageRevision struct {
	ImportID  string          `json:"import_id"`
	RevisedAt int64           `json:"revised_at"`
	Row       json.RawMessage `json:"row"`
}

// MessageRevisions returns every earlier version of a message, oldest
// first; none for a message no import has changed.
func (s *Store) MessageRevisions(id string) ([]MessageRevision, error) {
	rows, err := s.db.Query(`SELECT import_id, revised_at, row_json FROM message_revisions WHERE message_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []MessageRevision{}
	for rows.Next() {
		var revision MessageRevision
		var row string
		if err := rows.Scan(&revision.ImportID, &revision.RevisedAt, &row); err != nil {
			return nil, err
		}
		revision.Row = json.RawMessage(row)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) GetMessage(id string) (Message, error) {
	message, _, err := scanMessage(s.db.QueryRow("SELECT "+messageColumns+" "+messageJoin+" WHERE m.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		var supersededBy string
		switch lookupErr := s.db.QueryRow(`SELECT superseded_by FROM superseded_messages WHERE id = ?`, id).Scan(&supersededBy); {
		case lookupErr == nil:
			return Message{}, ErrSuperseded{ID: id, SupersededBy: supersededBy}
		case !errors.Is(lookupErr, sql.ErrNoRows):
			return Message{}, lookupErr
		}
		return Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	}
	return message, err
}

// ConversationFilter narrows GET /conversations.
type ConversationFilter struct {
	PersonID string
	// AccountID keeps the conversations the account takes part in: those
	// with a participant of its handle on its platform.
	AccountID string
	Platform  string
	Query     string // matches the title or a participant's display name
	Limit     int
}

// ListConversations answers GET /conversations, most recently active first.
func (s *Store) ListConversations(filter ConversationFilter) ([]Conversation, error) {
	if filter.Limit <= 0 || filter.Limit > 5000 {
		filter.Limit = 500
	}
	var where []string
	var args []any
	if filter.PersonID != "" {
		where = append(where, "c.id IN (SELECT conversation_id FROM participants WHERE person_id = ?)")
		args = append(args, filter.PersonID)
	}
	if filter.AccountID != "" {
		account, err := s.GetAccount(filter.AccountID)
		if err != nil {
			return nil, err
		}
		where = append(where, "c.platform = ? AND c.id IN (SELECT conversation_id FROM participants WHERE handle = ?)")
		args = append(args, account.Platform, account.Handle)
	}
	if filter.Platform != "" {
		where = append(where, "c.platform = ?")
		args = append(args, filter.Platform)
	}
	if filter.Query != "" {
		where = append(where, "(c.title LIKE ? OR c.id IN (SELECT conversation_id FROM participants WHERE display_name LIKE ?))")
		like := "%" + filter.Query + "%"
		args = append(args, like, like)
	}
	query := `SELECT c.id FROM conversations c LEFT JOIN (SELECT conversation_id, MAX(sent_at) AS last FROM messages GROUP BY conversation_id) l ON l.conversation_id = c.id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY COALESCE(l.last, 0) DESC, c.seq DESC LIMIT ?"
	args = append(args, filter.Limit)
	ids, err := s.queryStrings(query, args...)
	if err != nil {
		return nil, err
	}
	conversations := []Conversation{}
	for _, id := range ids {
		conversation, err := s.GetConversation(id)
		if err != nil {
			return nil, err
		}
		conversations = append(conversations, conversation)
	}
	return conversations, nil
}

func (s *Store) queryStrings(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// GetConversation reads one conversation with its participants and message
// summary.
func (s *Store) GetConversation(id string) (Conversation, error) {
	var conversation Conversation
	var isGroup int
	err := s.db.QueryRow(`SELECT id, platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at FROM conversations WHERE id = ?`, id).
		Scan(&conversation.ID, &conversation.Platform, &conversation.Source, &conversation.SourceRef, &conversation.Title, &isGroup, &conversation.ParentSourceRef, &conversation.CreatedAt, &conversation.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		into, lookupErr := s.mergedInto(id)
		if lookupErr != nil {
			return Conversation{}, lookupErr
		}
		if into != "" {
			return Conversation{}, ErrMerged{ID: id, MergedInto: into}
		}
		return Conversation{}, fmt.Errorf("%w: conversation %s", ErrNotFound, id)
	}
	if err != nil {
		return Conversation{}, err
	}
	conversation.IsGroup = isGroup == 1
	var first, last sql.NullInt64
	if err := s.db.QueryRow(`SELECT COUNT(*), MIN(sent_at), MAX(sent_at) FROM messages WHERE conversation_id = ?`, id).
		Scan(&conversation.MessageCount, &first, &last); err != nil {
		return Conversation{}, err
	}
	conversation.FirstMessageAt, conversation.LastMessageAt = first.Int64, last.Int64
	rows, err := s.db.Query(`SELECT handle, display_name, person_id, is_self FROM participants WHERE conversation_id = ? ORDER BY is_self DESC, display_name, handle`, id)
	if err != nil {
		return Conversation{}, err
	}
	defer rows.Close()
	conversation.Participants = []Participant{}
	for rows.Next() {
		var participant Participant
		var isSelf int
		if err := rows.Scan(&participant.Handle, &participant.DisplayName, &participant.PersonID, &isSelf); err != nil {
			return Conversation{}, err
		}
		participant.IsSelf = isSelf == 1
		conversation.Participants = append(conversation.Participants, participant)
	}
	return conversation, rows.Err()
}

const dayColumns = `d.id, d.conversation_id, d.day, d.time_zone, d.starts_at, d.ends_at, d.journal_source, d.journal_source_ref, d.journal_entry_id,
	(SELECT COUNT(*) FROM messages m WHERE m.conversation_id = d.conversation_id AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at), ` + dayInvolvementSQL

// dayInvolvementSQL is a conversation day d's involvement (DayInvolvements):
// direct for a chat that is not a group; in a group, participated when one of
// the user's handles there sent a message in the day's span, addressed when a
// message in it replies to one of the user's messages or mentions one of the
// user's handles the way Discord writes a mention (<@id>, <@!id>), else none.
const dayInvolvementSQL = `CASE
	WHEN (SELECT is_group FROM conversations c WHERE c.id = d.conversation_id) = 0 THEN 'direct'
	WHEN EXISTS (SELECT 1 FROM messages m JOIN participants p ON p.conversation_id = m.conversation_id AND p.handle = m.sender_handle
		WHERE m.conversation_id = d.conversation_id AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at AND p.is_self = 1) THEN 'participated'
	WHEN EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = d.conversation_id AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at AND (
		(m.reply_to_source_ref != '' AND EXISTS (SELECT 1 FROM messages own JOIN participants op ON op.conversation_id = own.conversation_id AND op.handle = own.sender_handle
			WHERE own.conversation_id = d.conversation_id AND own.source_ref = m.reply_to_source_ref AND op.is_self = 1))
		OR EXISTS (SELECT 1 FROM participants sp WHERE sp.conversation_id = d.conversation_id AND sp.is_self = 1 AND sp.handle != ''
			AND (instr(m.body, '<@' || sp.handle || '>') > 0 OR instr(m.body, '<@!' || sp.handle || '>') > 0)))) THEN 'addressed'
	ELSE 'none' END`

func scanDay(row interface{ Scan(...any) error }) (ConversationDay, error) {
	var day ConversationDay
	err := row.Scan(&day.ID, &day.ConversationID, &day.Day, &day.TimeZone, &day.StartsAt, &day.EndsAt, &day.JournalSource, &day.JournalSourceRef, &day.JournalEntryID, &day.MessageCount, &day.Involvement)
	return day, err
}

// DayFilter narrows GET /conversation-days.
type DayFilter struct {
	ConversationID string
	JournalEntryID string
	PersonID       string
	From, To       string // YYYY-MM-DD, inclusive
	// Involved keeps only days the user took part in (any involvement but none).
	Involved bool
	Limit    int
}

// ListConversationDays answers GET /conversation-days, in day order.
func (s *Store) ListConversationDays(filter DayFilter) ([]ConversationDay, error) {
	if filter.Limit <= 0 || filter.Limit > 10000 {
		filter.Limit = 1000
	}
	var where []string
	var args []any
	if filter.ConversationID != "" {
		live, err := s.liveConversationIDs([]string{filter.ConversationID})
		if err != nil {
			return nil, err
		}
		filter.ConversationID = live[0]
		where = append(where, "d.conversation_id = ?")
		args = append(args, filter.ConversationID)
	}
	if filter.JournalEntryID != "" {
		where = append(where, "d.journal_entry_id = ?")
		args = append(args, filter.JournalEntryID)
	}
	if filter.PersonID != "" {
		where = append(where, "d.conversation_id IN (SELECT conversation_id FROM participants WHERE person_id = ?)")
		args = append(args, filter.PersonID)
	}
	if filter.From != "" {
		where = append(where, "d.day >= ?")
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, "d.day <= ?")
		args = append(args, filter.To)
	}
	if filter.Involved {
		where = append(where, dayInvolvementSQL+" != 'none'")
	}
	query := "SELECT " + dayColumns + " FROM conversation_days d"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY d.day, d.seq LIMIT ?"
	args = append(args, filter.Limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []ConversationDay{}
	for rows.Next() {
		day, err := scanDay(rows)
		if err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

// GetConversationDay reads one day with its conversation and messages.
func (s *Store) GetConversationDay(id string) (ConversationDayWithMessages, error) {
	day, err := scanDay(s.db.QueryRow("SELECT "+dayColumns+" FROM conversation_days d WHERE d.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationDayWithMessages{}, fmt.Errorf("%w: conversation day %s", ErrNotFound, id)
	}
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	conversation, err := s.GetConversation(day.ConversationID)
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	page, err := s.ListMessages(MessageFilter{ConversationIDs: []string{day.ConversationID}, After: day.StartsAt, Before: day.EndsAt, Limit: 5000})
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	return ConversationDayWithMessages{ConversationDay: day, Conversation: conversation, Messages: page.Messages}, nil
}

// involvedGroupDays maps every group conversation to the local days, in
// location, on which the user took part in it: wrote a message, or had a
// message that replies to one of theirs or mentions one of their handles the
// way Discord writes a mention (<@id>, <@!id>). A group with no such day maps
// to an empty set, so all of it is left out; a conversation that is not a
// group has no entry.
func (s *Store) involvedGroupDays(location *time.Location) (map[string]map[string]bool, error) {
	days := map[string]map[string]bool{}
	groups, err := s.db.Query(`SELECT id FROM conversations WHERE is_group = 1`)
	if err != nil {
		return nil, err
	}
	for groups.Next() {
		var id string
		if err := groups.Scan(&id); err != nil {
			groups.Close()
			return nil, err
		}
		days[id] = map[string]bool{}
	}
	groups.Close()
	mark := func(conversationID string, sentAt int64) {
		days[conversationID][time.UnixMilli(sentAt).In(location).Format(time.DateOnly)] = true
	}
	// The user's own messages in groups: their days, and their refs, which a
	// reply names. Replies are matched here rather than by a self-join, which
	// SQLite plans as a scan per reply.
	own := map[string]map[string]bool{}
	ownRows, err := s.db.Query(`SELECT m.conversation_id, m.sent_at, m.source_ref FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.is_group = 1
		JOIN participants p ON p.conversation_id = m.conversation_id AND p.handle = m.sender_handle AND p.is_self = 1`)
	if err != nil {
		return nil, err
	}
	for ownRows.Next() {
		var conversationID, ref string
		var sentAt int64
		if err := ownRows.Scan(&conversationID, &sentAt, &ref); err != nil {
			ownRows.Close()
			return nil, err
		}
		mark(conversationID, sentAt)
		if own[conversationID] == nil {
			own[conversationID] = map[string]bool{}
		}
		own[conversationID][ref] = true
	}
	ownRows.Close()
	replyRows, err := s.db.Query(`SELECT m.conversation_id, m.sent_at, m.reply_to_source_ref FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.is_group = 1 WHERE m.reply_to_source_ref != ''`)
	if err != nil {
		return nil, err
	}
	for replyRows.Next() {
		var conversationID, repliesTo string
		var sentAt int64
		if err := replyRows.Scan(&conversationID, &sentAt, &repliesTo); err != nil {
			replyRows.Close()
			return nil, err
		}
		if own[conversationID][repliesTo] {
			mark(conversationID, sentAt)
		}
	}
	replyRows.Close()
	mentionRows, err := s.db.Query(`SELECT m.conversation_id, m.sent_at FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.is_group = 1
		JOIN participants sp ON sp.conversation_id = m.conversation_id AND sp.is_self = 1 AND sp.handle != ''
		WHERE instr(m.body, '<@' || sp.handle || '>') > 0 OR instr(m.body, '<@!' || sp.handle || '>') > 0`)
	if err != nil {
		return nil, err
	}
	defer mentionRows.Close()
	for mentionRows.Next() {
		var conversationID string
		var sentAt int64
		if err := mentionRows.Scan(&conversationID, &sentAt); err != nil {
			return nil, err
		}
		mark(conversationID, sentAt)
	}
	return days, mentionRows.Err()
}

// ActivityFilter is GET /activity.
type ActivityFilter struct {
	GroupBy  string
	Interval string
	TimeZone string
	PersonID string
	After    int64
	Before   int64
	// Involved counts only messages on days the user took part in: a group's
	// messages on a local day the user neither wrote, was replied to nor was
	// mentioned in are left out.
	Involved bool
}

// Activity counts messages per bucket of time per key.
//
// Grouped by person, a message counts for a person when that person sent it
// (received), or when the user sent it in a direct conversation with that
// person (sent). The user's messages in a group are not credited to anyone,
// since they were written to everyone there. Grouped by conversation or
// platform, every message counts.
func (s *Store) Activity(filter ActivityFilter) ([]ActivityBucket, error) {
	if !contains(ActivityGroupings, filter.GroupBy) {
		return nil, fmt.Errorf("%w: group_by %q is not one of %v", ErrInvalid, filter.GroupBy, ActivityGroupings)
	}
	if !contains(ActivityIntervals, filter.Interval) {
		return nil, fmt.Errorf("%w: interval %q is not one of %v", ErrInvalid, filter.Interval, ActivityIntervals)
	}
	location, err := time.LoadLocation(filter.TimeZone)
	if err != nil || filter.TimeZone == "" {
		return nil, fmt.Errorf("%w: time_zone %q is required and must be an IANA zone", ErrInvalid, filter.TimeZone)
	}
	// Each direct conversation's one other person.
	directPartner := map[string]string{}
	partnerRows, err := s.db.Query(`SELECT p.conversation_id, MIN(p.person_id) FROM participants p JOIN conversations c ON c.id = p.conversation_id
		WHERE c.is_group = 0 AND p.is_self = 0 AND p.person_id != '' GROUP BY p.conversation_id HAVING COUNT(DISTINCT p.person_id) = 1`)
	if err != nil {
		return nil, err
	}
	for partnerRows.Next() {
		var conversationID, personID string
		if err := partnerRows.Scan(&conversationID, &personID); err != nil {
			partnerRows.Close()
			return nil, err
		}
		directPartner[conversationID] = personID
	}
	partnerRows.Close()

	var where []string
	var args []any
	if filter.After > 0 {
		where = append(where, "m.sent_at >= ?")
		args = append(args, filter.After)
	}
	if filter.Before > 0 {
		where = append(where, "m.sent_at < ?")
		args = append(args, filter.Before)
	}
	if filter.PersonID != "" {
		where = append(where, "m.conversation_id IN (SELECT conversation_id FROM participants WHERE person_id = ?)")
		args = append(args, filter.PersonID)
	}
	// The local days, in the zone asked, on which the user took part in each
	// group: wrote there, or was replied to or mentioned. Read from the
	// messages themselves, so a chat with no conversation-day rows (multichat's
	// live log) is judged the same way.
	var involvedDays map[string]map[string]bool
	if filter.Involved {
		if involvedDays, err = s.involvedGroupDays(location); err != nil {
			return nil, err
		}
	}
	query := `SELECT m.sent_at, m.conversation_id, c.platform, COALESCE(p.person_id, ''), COALESCE(p.is_self, 0)
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN participants p ON p.conversation_id = m.conversation_id AND p.handle = m.sender_handle`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type bucketKey struct {
		start int64
		key   string
	}
	counts := map[bucketKey]*ActivityBucket{}
	var order []bucketKey
	for rows.Next() {
		var sentAt int64
		var conversationID, platform, senderPerson string
		var isSelf int
		if err := rows.Scan(&sentAt, &conversationID, &platform, &senderPerson, &isSelf); err != nil {
			return nil, err
		}
		if involvedDays != nil {
			if days, isGroup := involvedDays[conversationID]; isGroup && !days[time.UnixMilli(sentAt).In(location).Format(time.DateOnly)] {
				continue
			}
		}
		var key string
		switch filter.GroupBy {
		case "conversation":
			key = conversationID
		case "platform":
			key = platform
		case "person":
			if isSelf == 1 {
				key = directPartner[conversationID]
			} else {
				key = senderPerson
			}
		}
		if key == "" || (filter.GroupBy == "person" && filter.PersonID != "" && key != filter.PersonID) {
			continue
		}
		start := bucketStart(time.UnixMilli(sentAt).In(location), filter.Interval)
		bk := bucketKey{start, key}
		bucket := counts[bk]
		if bucket == nil {
			bucket = &ActivityBucket{Start: start, Key: key}
			counts[bk] = bucket
			order = append(order, bk)
		}
		if isSelf == 1 {
			bucket.Sent++
		} else {
			bucket.Received++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	buckets := make([]ActivityBucket, 0, len(order))
	for _, bk := range order {
		buckets = append(buckets, *counts[bk])
	}
	sortBuckets(buckets)
	return buckets, nil
}

func bucketStart(at time.Time, interval string) int64 {
	year, month, day := at.Date()
	location := at.Location()
	switch interval {
	case "day":
		return time.Date(year, month, day, 0, 0, 0, 0, location).UnixMilli()
	case "week":
		// Weeks start on Monday.
		offset := (int(at.Weekday()) + 6) % 7
		return time.Date(year, month, day-offset, 0, 0, 0, 0, location).UnixMilli()
	case "month":
		return time.Date(year, month, 1, 0, 0, 0, 0, location).UnixMilli()
	default:
		return time.Date(year, 1, 1, 0, 0, 0, 0, location).UnixMilli()
	}
}

// Summarize answers GET /summary.
func (s *Store) Summarize() (Summary, error) {
	rows, err := s.db.Query(`SELECT c.platform, COUNT(DISTINCT c.id), COUNT(m.seq), COALESCE(MIN(m.sent_at), 0), COALESCE(MAX(m.sent_at), 0)
		FROM conversations c LEFT JOIN messages m ON m.conversation_id = c.id GROUP BY c.platform ORDER BY c.platform`)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	summary := Summary{Platforms: []PlatformSummary{}}
	for rows.Next() {
		var platform PlatformSummary
		if err := rows.Scan(&platform.Platform, &platform.Conversations, &platform.Messages, &platform.FirstMessageAt, &platform.LastMessageAt); err != nil {
			return Summary{}, err
		}
		summary.Platforms = append(summary.Platforms, platform)
	}
	return summary, rows.Err()
}

// ErrSuperseded wraps the id of the message that now holds a retired one.
type ErrSuperseded struct{ ID, SupersededBy string }

func (e ErrSuperseded) Error() string {
	return fmt.Sprintf("message %s was superseded by %s", e.ID, e.SupersededBy)
}

const supersededColumns = `id, source, source_ref, superseded_by, import_id, superseded_at, row_json`

func scanSuperseded(row interface{ Scan(...any) error }) (SupersededMessage, error) {
	var superseded SupersededMessage
	var rowJSON string
	if err := row.Scan(&superseded.ID, &superseded.Source, &superseded.SourceRef, &superseded.SupersededBy, &superseded.ImportID,
		&superseded.SupersededAt, &rowJSON); err != nil {
		return SupersededMessage{}, err
	}
	superseded.Row = json.RawMessage(rowJSON)
	return superseded, nil
}

// ListSupersededMessages answers GET /superseded-messages, newest retirement
// first; supersededBy narrows it to the rows one message replaced.
func (s *Store) ListSupersededMessages(supersededBy string, limit int) ([]SupersededMessage, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := `SELECT ` + supersededColumns + ` FROM superseded_messages`
	var args []any
	if supersededBy != "" {
		query += ` WHERE superseded_by = ?`
		args = append(args, supersededBy)
	}
	rows, err := s.db.Query(query+` ORDER BY superseded_at DESC, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SupersededMessage{}
	for rows.Next() {
		superseded, err := scanSuperseded(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, superseded)
	}
	return out, rows.Err()
}

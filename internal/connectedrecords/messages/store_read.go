package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
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

const messageColumns = `m.seq, m.owner_principal_id, m.conversation_seq, m.sent_at, m.sender_handle, COALESCE(p.display_name, ''), COALESCE(p.person_id, ''), COALESCE(p.is_self, false),
	m.kind, m.body, m.body_source, m.attachments, m.embeds, m.buttons, m.reply_to_source_ref, m.call_seconds, m.edited, m.deleted_on_platform, m.source, m.source_ref, m.import_seq`

const messageJoin = `FROM messages m LEFT JOIN participants p ON p.conversation_seq = m.conversation_seq AND p.handle = m.sender_handle`

func scanMessage(rows interface{ Scan(...any) error }) (Message, int64, error) {
	var message Message
	var seq, conversationSeq int64
	var importSeq sql.NullInt64
	var attachments, embeds, buttons string
	if err := rows.Scan(&seq, &message.OwnerPrincipalID, &conversationSeq, &message.SentAt, &message.SenderHandle, &message.SenderName, &message.SenderPersonID, &message.FromSelf,
		&message.Kind, &message.Body, &message.BodySource, &attachments, &embeds, &buttons, &message.ReplyToSourceRef, &message.CallSeconds, &message.Edited, &message.DeletedOnPlatform,
		&message.Source, &message.SourceRef, &importSeq); err != nil {
		return Message{}, 0, err
	}
	message.ID = postgresstorage.FormatRecordID(MessageIDPrefix, seq)
	message.ConversationID = postgresstorage.FormatRecordID(ConversationIDPrefix, conversationSeq)
	message.ImportedBy = formatOptionalImport(importSeq)
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

// ListMessages answers GET /messages in scope: messages in time order, paged
// by a cursor of the last row's time and number.
func (s *Store) ListMessages(ctx context.Context, scope ownership.Scope, filter MessageFilter) (MessagePage, error) {
	if filter.Limit <= 0 || filter.Limit > 5000 {
		filter.Limit = 500
	}
	empty := MessagePage{Messages: []Message{}}
	args := &arguments{}
	condition, err := args.scope(scope, "m.owner_principal_id")
	if err != nil {
		return MessagePage{}, err
	}
	where := []string{condition}
	if len(filter.ConversationIDs) > 0 {
		seqs, err := s.liveConversationSeqs(ctx, scope, filter.ConversationIDs)
		if err != nil {
			return MessagePage{}, err
		}
		if len(seqs) == 0 {
			return empty, nil
		}
		where = append(where, "m.conversation_seq = ANY("+args.add(seqs)+"::bigint[])")
	}
	if filter.PersonID != "" {
		where = append(where, "m.conversation_seq IN (SELECT conversation_seq FROM participants WHERE person_id = "+args.add(filter.PersonID)+")")
	}
	if filter.Platform != "" {
		where = append(where, "m.conversation_seq IN (SELECT seq FROM conversations WHERE platform = "+args.add(filter.Platform)+")")
	}
	if filter.SourceRef != "" && filter.Source == "" {
		return MessagePage{}, fmt.Errorf("%w: source_ref needs source, since refs are only unique within one", ErrInvalid)
	}
	if filter.Source != "" {
		where = append(where, "m.source = "+args.add(filter.Source))
	}
	if filter.SourceRef != "" {
		where = append(where, "m.source_ref = "+args.add(filter.SourceRef))
	}
	if filter.After > 0 {
		where = append(where, "m.sent_at >= "+args.add(filter.After))
	}
	if filter.Before > 0 {
		where = append(where, "m.sent_at < "+args.add(filter.Before))
	}
	if filter.Query != "" {
		expression, searchable := searchQueryOf(filter.Query)
		if !searchable {
			return empty, nil
		}
		if expression != "" {
			where = append(where, "m.search_document @@ to_tsquery('simple', "+args.add(expression)+")")
		}
	}
	if filter.Cursor != "" {
		sentAt, seq, err := parseCursor(filter.Cursor)
		if err != nil {
			return MessagePage{}, err
		}
		sentAtPlaceholder, seqPlaceholder := args.add(sentAt), args.add(seq)
		if filter.Newest {
			where = append(where, "(m.sent_at < "+sentAtPlaceholder+" OR (m.sent_at = "+sentAtPlaceholder+" AND m.seq < "+seqPlaceholder+"))")
		} else {
			where = append(where, "(m.sent_at > "+sentAtPlaceholder+" OR (m.sent_at = "+sentAtPlaceholder+" AND m.seq > "+seqPlaceholder+"))")
		}
	}
	order := "m.sent_at ASC, m.seq ASC"
	if filter.Newest {
		order = "m.sent_at DESC, m.seq DESC"
	}
	query := "SELECT " + messageColumns + " " + messageJoin + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + order + " LIMIT " + args.add(filter.Limit+1)
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return MessagePage{}, err
	}
	defer rows.Close()
	page := empty
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

func parseCursor(cursor string) (int64, int64, error) {
	sentAtText, seqText, found := strings.Cut(cursor, ":")
	sentAt, err1 := strconv.ParseInt(sentAtText, 10, 64)
	seq, err2 := strconv.ParseInt(seqText, 10, 64)
	if !found || err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("%w: cursor %q", ErrInvalid, cursor)
	}
	return sentAt, seq, nil
}

// MessageRevision is a message as it was before an import changed it.
type MessageRevision struct {
	ImportID  string          `json:"import_id"`
	RevisedAt int64           `json:"revised_at"`
	Row       json.RawMessage `json:"row"`
}

// MessageRevisions returns every earlier version of a message in scope,
// oldest first; none for a message no import has changed, or one the scope
// does not reach.
func (s *Store) MessageRevisions(ctx context.Context, scope ownership.Scope, id string) ([]MessageRevision, error) {
	revisions := []MessageRevision{}
	seq, err := postgresstorage.ParseRecordID(MessageIDPrefix, id)
	if err != nil {
		return revisions, nil
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT import_seq, revised_at, row_json FROM message_revisions WHERE message_seq = `+args.add(seq)+` AND `+condition+` ORDER BY seq`, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var revision MessageRevision
		var importSeq int64
		var row string
		if err := rows.Scan(&importSeq, &revision.RevisedAt, &row); err != nil {
			return nil, err
		}
		revision.ImportID = postgresstorage.FormatRecordID(ImportIDPrefix, importSeq)
		revision.Row = json.RawMessage(row)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// GetMessage reads one message in scope. A retired id in scope answers
// ErrSuperseded naming the message that holds it now.
func (s *Store) GetMessage(ctx context.Context, scope ownership.Scope, id string) (Message, error) {
	seq, err := recordNumber(MessageIDPrefix, "message", id)
	if err != nil {
		return Message{}, err
	}
	args := &arguments{}
	condition, err := args.scope(scope, "m.owner_principal_id")
	if err != nil {
		return Message{}, err
	}
	message, _, err := scanMessage(s.db.QueryRowContext(ctx, "SELECT "+messageColumns+" "+messageJoin+" WHERE m.seq = "+args.add(seq)+" AND "+condition, args.values...))
	if errors.Is(err, sql.ErrNoRows) {
		retiredArgs := &arguments{}
		retiredCondition, err := retiredArgs.scope(scope, "owner_principal_id")
		if err != nil {
			return Message{}, err
		}
		var supersededBy int64
		switch lookupErr := s.db.QueryRowContext(ctx, `SELECT superseded_by_seq FROM superseded_messages WHERE message_seq = `+retiredArgs.add(seq)+` AND `+retiredCondition,
			retiredArgs.values...).Scan(&supersededBy); {
		case lookupErr == nil:
			return Message{}, ErrSuperseded{ID: id, SupersededBy: postgresstorage.FormatRecordID(MessageIDPrefix, supersededBy)}
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

// ListConversations answers GET /conversations in scope, most recently
// active first.
func (s *Store) ListConversations(ctx context.Context, scope ownership.Scope, filter ConversationFilter) ([]Conversation, error) {
	if filter.Limit <= 0 || filter.Limit > 5000 {
		filter.Limit = 500
	}
	args := &arguments{}
	condition, err := args.scope(scope, "c.owner_principal_id")
	if err != nil {
		return nil, err
	}
	where := []string{condition}
	if filter.PersonID != "" {
		where = append(where, "c.seq IN (SELECT conversation_seq FROM participants WHERE person_id = "+args.add(filter.PersonID)+")")
	}
	if filter.AccountID != "" {
		account, err := s.GetAccount(ctx, scope, filter.AccountID)
		if err != nil {
			return nil, err
		}
		// The account's conversations are its owner's: a handle it shares with
		// a chat someone else keeps says nothing about that chat.
		where = append(where, "c.owner_principal_id = "+args.add(account.OwnerPrincipalID)+" AND c.platform = "+args.add(account.Platform)+
			" AND c.seq IN (SELECT conversation_seq FROM participants WHERE handle = "+args.add(account.Handle)+")")
	}
	if filter.Platform != "" {
		where = append(where, "c.platform = "+args.add(filter.Platform))
	}
	if filter.Query != "" {
		// As SQLite's LIKE did: case blind, with % and _ in the query as
		// wildcards, and no escape character.
		like := args.add("%" + filter.Query + "%")
		where = append(where, "(c.title ILIKE "+like+" ESCAPE '' OR c.seq IN (SELECT conversation_seq FROM participants WHERE display_name ILIKE "+like+" ESCAPE ''))")
	}
	query := `SELECT c.seq FROM conversations c LEFT JOIN (SELECT conversation_seq, MAX(sent_at) AS last FROM messages GROUP BY conversation_seq) l ON l.conversation_seq = c.seq
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY COALESCE(l.last, 0) DESC, c.seq DESC LIMIT ` + args.add(filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	conversations := []Conversation{}
	for _, seq := range seqs {
		conversation, err := s.conversationBySeq(ctx, seq)
		if err != nil {
			return nil, err
		}
		conversations = append(conversations, conversation)
	}
	return conversations, nil
}

// GetConversation reads one conversation in scope with its participants and
// message summary. A merged id in scope answers ErrMerged naming the
// conversation it merged into.
func (s *Store) GetConversation(ctx context.Context, scope ownership.Scope, id string) (Conversation, error) {
	seq, err := recordNumber(ConversationIDPrefix, "conversation", id)
	if err != nil {
		return Conversation{}, err
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return Conversation{}, err
	}
	var found int64
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM conversations WHERE seq = `+args.add(seq)+` AND `+condition, args.values...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		into, lookupErr := s.mergedIntoSeq(ctx, scope, seq)
		if lookupErr != nil {
			return Conversation{}, lookupErr
		}
		if into != 0 {
			return Conversation{}, ErrMerged{ID: id, MergedInto: postgresstorage.FormatRecordID(ConversationIDPrefix, into)}
		}
		return Conversation{}, fmt.Errorf("%w: conversation %s", ErrNotFound, id)
	}
	if err != nil {
		return Conversation{}, err
	}
	return s.conversationBySeq(ctx, seq)
}

// conversationBySeq reads one conversation the caller already holds in
// scope.
func (s *Store) conversationBySeq(ctx context.Context, seq int64) (Conversation, error) {
	var conversation Conversation
	err := s.db.QueryRowContext(ctx, `SELECT owner_principal_id, platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at FROM conversations WHERE seq = $1`, seq).
		Scan(&conversation.OwnerPrincipalID, &conversation.Platform, &conversation.Source, &conversation.SourceRef, &conversation.Title, &conversation.IsGroup,
			&conversation.ParentSourceRef, &conversation.CreatedAt, &conversation.UpdatedAt)
	if err != nil {
		return Conversation{}, err
	}
	conversation.ID = postgresstorage.FormatRecordID(ConversationIDPrefix, seq)
	var first, last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(sent_at), MAX(sent_at) FROM messages WHERE conversation_seq = $1`, seq).
		Scan(&conversation.MessageCount, &first, &last); err != nil {
		return Conversation{}, err
	}
	conversation.FirstMessageAt, conversation.LastMessageAt = first.Int64, last.Int64
	// The standalone store sorted with SQLite's binary collation; COLLATE "C"
	// sorts the same, by byte.
	rows, err := s.db.QueryContext(ctx, `SELECT handle, display_name, person_id, is_self FROM participants WHERE conversation_seq = $1
		ORDER BY is_self DESC, display_name COLLATE "C", handle COLLATE "C"`, seq)
	if err != nil {
		return Conversation{}, err
	}
	defer rows.Close()
	conversation.Participants = []Participant{}
	for rows.Next() {
		var participant Participant
		if err := rows.Scan(&participant.Handle, &participant.DisplayName, &participant.PersonID, &participant.IsSelf); err != nil {
			return Conversation{}, err
		}
		conversation.Participants = append(conversation.Participants, participant)
	}
	return conversation, rows.Err()
}

const dayColumns = `d.seq, d.owner_principal_id, d.conversation_seq, d.day, d.time_zone, d.starts_at, d.ends_at, d.journal_source, d.journal_source_ref, d.journal_entry_id,
	(SELECT COUNT(*) FROM messages m WHERE m.conversation_seq = d.conversation_seq AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at), ` + dayInvolvementSQL

// dayInvolvementSQL is a conversation day d's involvement (DayInvolvements):
// direct for a chat that is not a group; in a group, participated when one of
// the user's handles there sent a message in the day's span, addressed when a
// message in it replies to one of the user's messages or mentions one of the
// user's handles the way Discord writes a mention (<@id>, <@!id>), else none.
const dayInvolvementSQL = `CASE
	WHEN NOT (SELECT is_group FROM conversations c WHERE c.seq = d.conversation_seq) THEN 'direct'
	WHEN EXISTS (SELECT 1 FROM messages m JOIN participants p ON p.conversation_seq = m.conversation_seq AND p.handle = m.sender_handle
		WHERE m.conversation_seq = d.conversation_seq AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at AND p.is_self) THEN 'participated'
	WHEN EXISTS (SELECT 1 FROM messages m WHERE m.conversation_seq = d.conversation_seq AND m.sent_at >= d.starts_at AND m.sent_at < d.ends_at AND (
		(m.reply_to_source_ref <> '' AND EXISTS (SELECT 1 FROM messages own JOIN participants op ON op.conversation_seq = own.conversation_seq AND op.handle = own.sender_handle
			WHERE own.conversation_seq = d.conversation_seq AND own.source_ref = m.reply_to_source_ref AND op.is_self))
		OR EXISTS (SELECT 1 FROM participants sp WHERE sp.conversation_seq = d.conversation_seq AND sp.is_self AND sp.handle <> ''
			AND (strpos(m.body, '<@' || sp.handle || '>') > 0 OR strpos(m.body, '<@!' || sp.handle || '>') > 0)))) THEN 'addressed'
	ELSE 'none' END`

func scanDay(row interface{ Scan(...any) error }) (ConversationDay, error) {
	var day ConversationDay
	var seq, conversationSeq int64
	err := row.Scan(&seq, &day.OwnerPrincipalID, &conversationSeq, &day.Day, &day.TimeZone, &day.StartsAt, &day.EndsAt, &day.JournalSource, &day.JournalSourceRef,
		&day.JournalEntryID, &day.MessageCount, &day.Involvement)
	day.ID = postgresstorage.FormatRecordID(ConversationDayIDPrefix, seq)
	day.ConversationID = postgresstorage.FormatRecordID(ConversationIDPrefix, conversationSeq)
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

// ListConversationDays answers GET /conversation-days in scope, in day
// order.
func (s *Store) ListConversationDays(ctx context.Context, scope ownership.Scope, filter DayFilter) ([]ConversationDay, error) {
	if filter.Limit <= 0 || filter.Limit > 10000 {
		filter.Limit = 1000
	}
	args := &arguments{}
	condition, err := args.scope(scope, "d.owner_principal_id")
	if err != nil {
		return nil, err
	}
	where := []string{condition}
	if filter.ConversationID != "" {
		seqs, err := s.liveConversationSeqs(ctx, scope, []string{filter.ConversationID})
		if err != nil {
			return nil, err
		}
		if len(seqs) == 0 {
			return []ConversationDay{}, nil
		}
		where = append(where, "d.conversation_seq = "+args.add(seqs[0]))
	}
	if filter.JournalEntryID != "" {
		where = append(where, "d.journal_entry_id = "+args.add(filter.JournalEntryID))
	}
	if filter.PersonID != "" {
		where = append(where, "d.conversation_seq IN (SELECT conversation_seq FROM participants WHERE person_id = "+args.add(filter.PersonID)+")")
	}
	if filter.From != "" {
		where = append(where, `d.day COLLATE "C" >= `+args.add(filter.From))
	}
	if filter.To != "" {
		where = append(where, `d.day COLLATE "C" <= `+args.add(filter.To))
	}
	if filter.Involved {
		where = append(where, dayInvolvementSQL+" <> 'none'")
	}
	query := "SELECT " + dayColumns + " FROM conversation_days d WHERE " + strings.Join(where, " AND ") +
		` ORDER BY d.day COLLATE "C", d.seq LIMIT ` + args.add(filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args.values...)
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

// GetConversationDay reads one day in scope with its conversation and
// messages.
func (s *Store) GetConversationDay(ctx context.Context, scope ownership.Scope, id string) (ConversationDayWithMessages, error) {
	seq, err := recordNumber(ConversationDayIDPrefix, "conversation day", id)
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	args := &arguments{}
	condition, err := args.scope(scope, "d.owner_principal_id")
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	day, err := scanDay(s.db.QueryRowContext(ctx, "SELECT "+dayColumns+" FROM conversation_days d WHERE d.seq = "+args.add(seq)+" AND "+condition, args.values...))
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationDayWithMessages{}, fmt.Errorf("%w: conversation day %s", ErrNotFound, id)
	}
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	owner := ownership.OwnedBy(day.OwnerPrincipalID)
	conversation, err := s.GetConversation(ctx, owner, day.ConversationID)
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	page, err := s.ListMessages(ctx, owner, MessageFilter{ConversationIDs: []string{day.ConversationID}, After: day.StartsAt, Before: day.EndsAt, Limit: 5000})
	if err != nil {
		return ConversationDayWithMessages{}, err
	}
	return ConversationDayWithMessages{ConversationDay: day, Conversation: conversation, Messages: page.Messages}, nil
}

// involvedGroupDays maps every group conversation in scope to the local
// days, in location, on which the user took part in it: wrote a message, or
// had a message that replies to one of theirs or mentions one of their
// handles the way Discord writes a mention (<@id>, <@!id>). A group with no
// such day maps to an empty set, so all of it is left out; a conversation
// that is not a group has no entry.
func (s *Store) involvedGroupDays(ctx context.Context, scope ownership.Scope, location *time.Location) (map[int64]map[string]bool, error) {
	days := map[int64]map[string]bool{}
	args := &arguments{}
	condition, err := args.scope(scope, "c.owner_principal_id")
	if err != nil {
		return nil, err
	}
	groups, err := s.db.QueryContext(ctx, `SELECT c.seq FROM conversations c WHERE c.is_group AND `+condition, args.values...)
	if err != nil {
		return nil, err
	}
	for groups.Next() {
		var seq int64
		if err := groups.Scan(&seq); err != nil {
			groups.Close()
			return nil, err
		}
		days[seq] = map[string]bool{}
	}
	groups.Close()
	if err := groups.Err(); err != nil {
		return nil, err
	}
	mark := func(conversationSeq, sentAt int64) {
		days[conversationSeq][time.UnixMilli(sentAt).In(location).Format(time.DateOnly)] = true
	}
	// The user's own messages in groups: their days, and their refs, which a
	// reply names.
	own := map[int64]map[string]bool{}
	ownRows, err := s.db.QueryContext(ctx, `SELECT m.conversation_seq, m.sent_at, m.source_ref FROM messages m
		JOIN conversations c ON c.seq = m.conversation_seq AND c.is_group
		JOIN participants p ON p.conversation_seq = m.conversation_seq AND p.handle = m.sender_handle AND p.is_self
		WHERE `+condition, args.values...)
	if err != nil {
		return nil, err
	}
	for ownRows.Next() {
		var conversationSeq, sentAt int64
		var ref string
		if err := ownRows.Scan(&conversationSeq, &sentAt, &ref); err != nil {
			ownRows.Close()
			return nil, err
		}
		mark(conversationSeq, sentAt)
		if own[conversationSeq] == nil {
			own[conversationSeq] = map[string]bool{}
		}
		own[conversationSeq][ref] = true
	}
	ownRows.Close()
	if err := ownRows.Err(); err != nil {
		return nil, err
	}
	replyRows, err := s.db.QueryContext(ctx, `SELECT m.conversation_seq, m.sent_at, m.reply_to_source_ref FROM messages m
		JOIN conversations c ON c.seq = m.conversation_seq AND c.is_group WHERE m.reply_to_source_ref <> '' AND `+condition, args.values...)
	if err != nil {
		return nil, err
	}
	for replyRows.Next() {
		var conversationSeq, sentAt int64
		var repliesTo string
		if err := replyRows.Scan(&conversationSeq, &sentAt, &repliesTo); err != nil {
			replyRows.Close()
			return nil, err
		}
		if own[conversationSeq][repliesTo] {
			mark(conversationSeq, sentAt)
		}
	}
	replyRows.Close()
	if err := replyRows.Err(); err != nil {
		return nil, err
	}
	mentionRows, err := s.db.QueryContext(ctx, `SELECT m.conversation_seq, m.sent_at FROM messages m
		JOIN conversations c ON c.seq = m.conversation_seq AND c.is_group
		JOIN participants sp ON sp.conversation_seq = m.conversation_seq AND sp.is_self AND sp.handle <> ''
		WHERE (strpos(m.body, '<@' || sp.handle || '>') > 0 OR strpos(m.body, '<@!' || sp.handle || '>') > 0) AND `+condition, args.values...)
	if err != nil {
		return nil, err
	}
	defer mentionRows.Close()
	for mentionRows.Next() {
		var conversationSeq, sentAt int64
		if err := mentionRows.Scan(&conversationSeq, &sentAt); err != nil {
			return nil, err
		}
		mark(conversationSeq, sentAt)
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

// Activity counts the messages in scope per bucket of time per key.
//
// Grouped by person, a message counts for a person when that person sent it
// (received), or when the user sent it in a direct conversation with that
// person (sent). The user's messages in a group are not credited to anyone,
// since they were written to everyone there. Grouped by conversation or
// platform, every message counts.
func (s *Store) Activity(ctx context.Context, scope ownership.Scope, filter ActivityFilter) ([]ActivityBucket, error) {
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
	partnerArgs := &arguments{}
	partnerCondition, err := partnerArgs.scope(scope, "c.owner_principal_id")
	if err != nil {
		return nil, err
	}
	directPartner := map[int64]string{}
	partnerRows, err := s.db.QueryContext(ctx, `SELECT p.conversation_seq, MIN(p.person_id COLLATE "C") FROM participants p JOIN conversations c ON c.seq = p.conversation_seq
		WHERE NOT c.is_group AND NOT p.is_self AND p.person_id <> '' AND `+partnerCondition+` GROUP BY p.conversation_seq HAVING COUNT(DISTINCT p.person_id) = 1`,
		partnerArgs.values...)
	if err != nil {
		return nil, err
	}
	for partnerRows.Next() {
		var conversationSeq int64
		var personID string
		if err := partnerRows.Scan(&conversationSeq, &personID); err != nil {
			partnerRows.Close()
			return nil, err
		}
		directPartner[conversationSeq] = personID
	}
	partnerRows.Close()
	if err := partnerRows.Err(); err != nil {
		return nil, err
	}

	args := &arguments{}
	condition, err := args.scope(scope, "m.owner_principal_id")
	if err != nil {
		return nil, err
	}
	where := []string{condition}
	if filter.After > 0 {
		where = append(where, "m.sent_at >= "+args.add(filter.After))
	}
	if filter.Before > 0 {
		where = append(where, "m.sent_at < "+args.add(filter.Before))
	}
	if filter.PersonID != "" {
		where = append(where, "m.conversation_seq IN (SELECT conversation_seq FROM participants WHERE person_id = "+args.add(filter.PersonID)+")")
	}
	// The local days, in the zone asked, on which the user took part in each
	// group: wrote there, or was replied to or mentioned. Read from the
	// messages themselves, so a chat with no conversation-day rows (multichat's
	// live log) is judged the same way.
	var involvedDays map[int64]map[string]bool
	if filter.Involved {
		if involvedDays, err = s.involvedGroupDays(ctx, scope, location); err != nil {
			return nil, err
		}
	}
	query := `SELECT m.sent_at, m.conversation_seq, c.platform, COALESCE(p.person_id, ''), COALESCE(p.is_self, false)
		FROM messages m JOIN conversations c ON c.seq = m.conversation_seq
		LEFT JOIN participants p ON p.conversation_seq = m.conversation_seq AND p.handle = m.sender_handle
		WHERE ` + strings.Join(where, " AND ")
	rows, err := s.db.QueryContext(ctx, query, args.values...)
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
		var sentAt, conversationSeq int64
		var platform, senderPerson string
		var isSelf bool
		if err := rows.Scan(&sentAt, &conversationSeq, &platform, &senderPerson, &isSelf); err != nil {
			return nil, err
		}
		if involvedDays != nil {
			if days, isGroup := involvedDays[conversationSeq]; isGroup && !days[time.UnixMilli(sentAt).In(location).Format(time.DateOnly)] {
				continue
			}
		}
		var key string
		switch filter.GroupBy {
		case "conversation":
			key = postgresstorage.FormatRecordID(ConversationIDPrefix, conversationSeq)
		case "platform":
			key = platform
		case "person":
			if isSelf {
				key = directPartner[conversationSeq]
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
		if isSelf {
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

// Summarize answers GET /summary over the records in scope.
func (s *Store) Summarize(ctx context.Context, scope ownership.Scope) (Summary, error) {
	args := &arguments{}
	condition, err := args.scope(scope, "c.owner_principal_id")
	if err != nil {
		return Summary{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.platform, COUNT(DISTINCT c.seq), COUNT(m.seq), COALESCE(MIN(m.sent_at), 0), COALESCE(MAX(m.sent_at), 0)
		FROM conversations c LEFT JOIN messages m ON m.conversation_seq = c.seq WHERE `+condition+` GROUP BY c.platform ORDER BY c.platform COLLATE "C"`, args.values...)
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

const supersededColumns = `message_seq, owner_principal_id, source, source_ref, superseded_by_seq, import_seq, superseded_at, row_json`

func scanSuperseded(row interface{ Scan(...any) error }) (SupersededMessage, error) {
	var superseded SupersededMessage
	var seq, bySeq, importSeq int64
	var rowJSON string
	if err := row.Scan(&seq, &superseded.OwnerPrincipalID, &superseded.Source, &superseded.SourceRef, &bySeq, &importSeq,
		&superseded.SupersededAt, &rowJSON); err != nil {
		return SupersededMessage{}, err
	}
	superseded.ID = postgresstorage.FormatRecordID(MessageIDPrefix, seq)
	superseded.SupersededBy = postgresstorage.FormatRecordID(MessageIDPrefix, bySeq)
	superseded.ImportID = postgresstorage.FormatRecordID(ImportIDPrefix, importSeq)
	superseded.Row = json.RawMessage(rowJSON)
	return superseded, nil
}

// ListSupersededMessages answers GET /superseded-messages in scope, newest
// retirement first; supersededBy narrows it to the rows one message
// replaced.
func (s *Store) ListSupersededMessages(ctx context.Context, scope ownership.Scope, supersededBy string, limit int) ([]SupersededMessage, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + supersededColumns + ` FROM superseded_messages WHERE ` + condition
	if supersededBy != "" {
		bySeq, err := postgresstorage.ParseRecordID(MessageIDPrefix, supersededBy)
		if err != nil {
			return []SupersededMessage{}, nil
		}
		query += ` AND superseded_by_seq = ` + args.add(bySeq)
	}
	// The standalone store broke ties on the text id; the number orders the
	// same for every id of six digits.
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY superseded_at DESC, message_seq LIMIT `+args.add(limit), args.values...)
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

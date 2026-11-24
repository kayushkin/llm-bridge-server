package projectstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// A session filing is what the session filer (session_filer.go) concluded
// about one chat session: it filed the session under a project by itself, it
// proposed a project, or none, for a person to decide, or it found nothing to
// file. Every judgment is a row, so a link the filer made can be traced to the
// evidence, the model and the odds behind it, and so a session is judged again
// only when its goal changes.

// Statuses of a session filing.
const (
	// SessionFilingFiled: the filer was sure enough and filed the session.
	SessionFilingFiled = "filed"
	// SessionFilingProposed: no project fit well enough; a person decides.
	SessionFilingProposed = "proposed"
	// SessionFilingAccepted: a person filed the session through the proposal.
	SessionFilingAccepted = "accepted"
	// SessionFilingDismissed: a person said to leave the session unfiled.
	SessionFilingDismissed = "dismissed"
	// SessionFilingSuperseded: a later judgment, or someone filing the
	// session directly, replaced the proposal before anyone decided on it.
	SessionFilingSuperseded = "superseded"
	// SessionFilingNothingToFile: the chat has no name and no brief, so
	// nothing was said in it.
	SessionFilingNothingToFile = "nothing_to_file"
)

// SessionFilingStatuses lists every status.
var SessionFilingStatuses = []string{SessionFilingFiled, SessionFilingProposed, SessionFilingAccepted, SessionFilingDismissed,
	SessionFilingSuperseded, SessionFilingNothingToFile}

// What the filer read about the session.
const (
	// SessionFilingEvidenceBrief: llm-bridge-server's brief of the chat.
	SessionFilingEvidenceBrief = "brief"
	// SessionFilingEvidenceName: the chat has no brief, so only its name, its
	// folder and the repos it changed.
	SessionFilingEvidenceName = "name"
	// SessionFilingEvidenceNone: neither a brief nor a name.
	SessionFilingEvidenceNone = "none"
)

// SessionFilingEvidenceKinds lists every kind of evidence.
var SessionFilingEvidenceKinds = []string{SessionFilingEvidenceBrief, SessionFilingEvidenceName, SessionFilingEvidenceNone}

// SessionFiling is one judgment of one session.
type SessionFiling struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	// SessionLabel is the session's name when it was judged, for display only.
	SessionLabel string `json:"session_label"`
	Evidence     string `json:"evidence"`
	// Goal is the brief's overall goal as judged; "" when the session had no brief.
	Goal string `json:"goal"`
	// RepoStoreIDs are the repos the session moved refs in, by repo-store's id.
	RepoStoreIDs []int64 `json:"repo_store_ids"`
	// ProjectID is the project the model picked; "" when none fits.
	ProjectID string `json:"project_id"`
	// Confidence is the model's odds, 0 to 1, that ProjectID is right.
	Confidence float64 `json:"confidence"`
	Why        string  `json:"why"`
	// Model is the model that answered; "" when no model was asked.
	Model  string `json:"model"`
	Status string `json:"status"`
	// LinkID is the link a filed or accepted filing made or found; 0 otherwise.
	LinkID int64 `json:"link_id"`
	// FiledProjectID is the project of LinkID: ProjectID when the filer
	// filed, the person's choice when they accepted.
	FiledProjectID string `json:"filed_project_id"`
	CreatedBy      string `json:"created_by"`
	CreatedAt      int64  `json:"created_at"`
	DecidedBy      string `json:"decided_by"`
	DecidedAt      int64  `json:"decided_at"`
}

// SessionFilerFailure is one session a pass could not judge.
type SessionFilerFailure struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
}

// SessionFilerReport says what one pass saw and did.
type SessionFilerReport struct {
	// Since is the start of the window, unix seconds.
	Since int64 `json:"since"`
	// ChatSessions is how many chats llm-bridge-server changed in the window.
	ChatSessions int `json:"chat_sessions"`
	// FiledByOthers counts chats a person or another agent filed; the filer
	// leaves those alone.
	FiledByOthers int `json:"filed_by_others"`
	// AlreadyJudged counts chats the filer judged before whose goal has not
	// changed since, by the status of that filing.
	AlreadyJudged map[string]int `json:"already_judged"`
	// Skipped counts sessions the filer does not file, by reason.
	Skipped map[string]int `json:"skipped"`
	// WaitingForABrief counts chats with no brief yet that changed too
	// recently to be judged from their name.
	WaitingForABrief int `json:"waiting_for_a_brief"`
	// Judged are the filings this pass made.
	Judged []SessionFiling `json:"judged"`
	// Failed are the sessions this pass could not judge; a later pass tries again.
	Failed []SessionFilerFailure `json:"failed"`
	// Remaining counts chats that still need judging because the pass
	// reached its limit.
	Remaining int `json:"remaining"`
}

const sessionFilingColumns = `number, session_id, session_label, evidence, goal, repo_store_ids, project_number, confidence, why, model, status, link_id, filed_project_number, created_by, created_at, decided_by, decided_at`

func scanSessionFiling(row scanner) (SessionFiling, error) {
	var filing SessionFiling
	var number int64
	var repoStoreIDs []byte
	var projectNumber, filedProjectNumber sql.NullInt64
	if err := row.Scan(&number, &filing.SessionID, &filing.SessionLabel, &filing.Evidence, &filing.Goal, &repoStoreIDs, &projectNumber,
		&filing.Confidence, &filing.Why, &filing.Model, &filing.Status, &filing.LinkID, &filedProjectNumber, &filing.CreatedBy,
		&filing.CreatedAt, &filing.DecidedBy, &filing.DecidedAt); err != nil {
		return SessionFiling{}, err
	}
	filing.ID = postgresstorage.FormatRecordID(SessionFilingIDPrefix, number)
	filing.ProjectID = projectIDOf(projectNumber)
	filing.FiledProjectID = projectIDOf(filedProjectNumber)
	if err := json.Unmarshal(repoStoreIDs, &filing.RepoStoreIDs); err != nil {
		return SessionFiling{}, fmt.Errorf("session filing %s: repo_store_ids %q: %w", filing.ID, repoStoreIDs, err)
	}
	if filing.RepoStoreIDs == nil {
		filing.RepoStoreIDs = []int64{}
	}
	return filing, nil
}

// insertLinkOrFindIt writes a link inside a transaction, or returns the link
// already there when the project owns the entity.
func insertLinkOrFindIt(transaction *sql.Tx, link Link) (Link, error) {
	created, inserted, err := insertLink(transaction, link)
	if err != nil || inserted {
		return created, err
	}
	number, err := projectNumberOf(link.ProjectID)
	if err != nil {
		return Link{}, err
	}
	return scanLink(transaction.QueryRow(`SELECT `+linkColumns+` FROM project_links WHERE project_number = $1 AND entity_type = $2 AND entity_ref = $3`,
		number, link.EntityType, link.EntityRef))
}

// sessionFilingLinkNote is the note on a link the filer made: why, how sure,
// and the filing that holds the rest.
func sessionFilingLinkNote(filing SessionFiling) string {
	return fmt.Sprintf("%s (%d%% sure, %s)", filing.Why, int(filing.Confidence*100+0.5), filing.ID)
}

// RecordSessionFiling stores one judgment. With status filed it also files the
// session under filing.ProjectID in the same transaction, so no link of the
// filer's exists without the judgment behind it. The session's earlier
// proposal that nobody decided on becomes superseded.
func (store *Store) RecordSessionFiling(filing SessionFiling) (SessionFiling, error) {
	if !contains(SessionFilingStatuses, filing.Status) || !contains(SessionFilingEvidenceKinds, filing.Evidence) {
		return SessionFiling{}, fmt.Errorf("%w: a session filing needs a known status and evidence, got %q and %q", ErrInvalid, filing.Status, filing.Evidence)
	}
	if filing.SessionID == "" {
		return SessionFiling{}, fmt.Errorf("%w: a session filing needs a session id", ErrInvalid)
	}
	if filing.RepoStoreIDs == nil {
		filing.RepoStoreIDs = []int64{}
	}
	repoStoreIDs, err := json.Marshal(filing.RepoStoreIDs)
	if err != nil {
		return SessionFiling{}, err
	}
	err = store.inTransaction(func(transaction *sql.Tx) error {
		now := store.now().Unix()
		filing.CreatedAt = now
		projectNumber, err := nullableProjectNumber(filing.ProjectID)
		if err != nil {
			return err
		}
		if filing.Status == SessionFilingFiled {
			var archivedAt int64
			err := transaction.QueryRow(`SELECT archived_at FROM projects WHERE number = $1`, projectNumber).Scan(&archivedAt)
			if errors.Is(err, sql.ErrNoRows) || projectNumber == nil {
				return fmt.Errorf("%w: project %q does not exist, so nothing can be filed under it", ErrInvalid, filing.ProjectID)
			} else if err != nil {
				return err
			}
		}
		if _, err := transaction.Exec(`UPDATE session_filings SET status = $1, decided_by = $2, decided_at = $3 WHERE session_id = $4 AND status = $5`,
			SessionFilingSuperseded, filing.CreatedBy, now, filing.SessionID, SessionFilingProposed); err != nil {
			return err
		}
		var number int64
		if err := transaction.QueryRow(`INSERT INTO session_filings (session_id, session_label, evidence, goal, repo_store_ids, project_number, confidence, why, model, status, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING number`,
			filing.SessionID, filing.SessionLabel, filing.Evidence, filing.Goal, string(repoStoreIDs), projectNumber, filing.Confidence,
			filing.Why, filing.Model, filing.Status, filing.CreatedBy, now).Scan(&number); err != nil {
			return err
		}
		filing.ID = postgresstorage.FormatRecordID(SessionFilingIDPrefix, number)
		if filing.Status != SessionFilingFiled {
			return nil
		}
		// The link's note names the filing, so the filing is written first and
		// told its link after, in the same transaction.
		link, err := insertLinkOrFindIt(transaction, Link{ProjectID: filing.ProjectID, EntityType: EntitySession, EntityRef: filing.SessionID,
			Label: filing.SessionLabel, Note: sessionFilingLinkNote(filing), CreatedBy: filing.CreatedBy, CreatedAt: now})
		if err != nil {
			return err
		}
		_, err = transaction.Exec(`UPDATE session_filings SET link_id = $1, filed_project_number = $2 WHERE number = $3`, link.ID, projectNumber, number)
		return err
	})
	if err != nil {
		return SessionFiling{}, err
	}
	return store.SessionFiling(filing.ID)
}

// SessionFiling returns one session filing.
func (store *Store) SessionFiling(id string) (SessionFiling, error) {
	return sessionFilingOn(store.database, id, false)
}

func sessionFilingOn(handle queryer, id string, forUpdate bool) (SessionFiling, error) {
	number, ok := recordNumberOf(SessionFilingIDPrefix, id)
	if !ok {
		return SessionFiling{}, fmt.Errorf("session filing %s: %w", id, ErrNotFound)
	}
	query := `SELECT ` + sessionFilingColumns + ` FROM session_filings WHERE number = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	filing, err := scanSessionFiling(handle.QueryRow(query, number))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionFiling{}, fmt.Errorf("session filing %s: %w", id, ErrNotFound)
	}
	return filing, err
}

// SessionFilingFilter narrows SessionFilings. Zero fields do not filter.
type SessionFilingFilter struct {
	Status    string
	SessionID string
	// ProjectID matches the project picked or the project filed under.
	ProjectID string
	// Limit caps the rows; 0 means no cap.
	Limit int
}

// SessionFilings lists session filings, newest first.
func (store *Store) SessionFilings(filter SessionFilingFilter) ([]SessionFiling, error) {
	if filter.Status != "" && !contains(SessionFilingStatuses, filter.Status) {
		return nil, fmt.Errorf("%w: status %q is not one of %s", ErrInvalid, filter.Status, strings.Join(SessionFilingStatuses, ", "))
	}
	var conditions []string
	var arguments []any
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = "+placeholder(filter.Status))
	}
	if filter.SessionID != "" {
		conditions = append(conditions, "session_id = "+placeholder(filter.SessionID))
	}
	if filter.ProjectID != "" {
		projectNumber, ok := recordNumberOf(ProjectIDPrefix, filter.ProjectID)
		if !ok {
			return []SessionFiling{}, nil
		}
		number := placeholder(projectNumber)
		conditions = append(conditions, "(project_number = "+number+" OR filed_project_number = "+number+")")
	}
	query := `SELECT ` + sessionFilingColumns + ` FROM session_filings`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY number DESC"
	if filter.Limit > 0 {
		query += " LIMIT " + placeholder(filter.Limit)
	}
	return store.querySessionFilings(query, arguments...)
}

func (store *Store) querySessionFilings(query string, arguments ...any) ([]SessionFiling, error) {
	rows, err := store.database.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	filings := []SessionFiling{}
	for rows.Next() {
		filing, err := scanSessionFiling(rows)
		if err != nil {
			return nil, err
		}
		filings = append(filings, filing)
	}
	return filings, rows.Err()
}

// LatestSessionFilings returns each judged session's newest filing, by
// session id.
func (store *Store) LatestSessionFilings() (map[string]SessionFiling, error) {
	filings, err := store.querySessionFilings(`SELECT ` + sessionFilingColumns + ` FROM session_filings
		WHERE number IN (SELECT MAX(number) FROM session_filings GROUP BY session_id)`)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]SessionFiling, len(filings))
	for _, filing := range filings {
		latest[filing.SessionID] = filing
	}
	return latest, nil
}

// AcceptSessionFiling files a proposed session under projectID and records
// who decided. The caller has checked the session with llm-bridge-server and
// passes its name as label. A filing already decided is a conflict.
func (store *Store) AcceptSessionFiling(id, projectID, decidedBy, label string) (SessionFiling, Link, error) {
	var link Link
	err := store.inTransaction(func(transaction *sql.Tx) error {
		filing, err := sessionFilingOn(transaction, id, true)
		if err != nil {
			return err
		}
		if filing.Status != SessionFilingProposed {
			return fmt.Errorf("%w: session filing %s is already %s", ErrConflict, id, filing.Status)
		}
		projectNumber, ok := recordNumberOf(ProjectIDPrefix, projectID)
		var projectExists int
		if ok {
			if err := transaction.QueryRow(`SELECT COUNT(*) FROM projects WHERE number = $1`, projectNumber).Scan(&projectExists); err != nil {
				return err
			}
		}
		if projectExists == 0 {
			return fmt.Errorf("%w: project_id %q names no project", ErrInvalid, projectID)
		}
		now := store.now().Unix()
		link, err = insertLinkOrFindIt(transaction, Link{ProjectID: projectID, EntityType: EntitySession, EntityRef: filing.SessionID, Label: label,
			Note: "accepted " + filing.ID, CreatedBy: decidedBy, CreatedAt: now})
		if err != nil {
			return err
		}
		filingNumber, _ := recordNumberOf(SessionFilingIDPrefix, id)
		_, err = transaction.Exec(`UPDATE session_filings SET status = $1, link_id = $2, filed_project_number = $3, decided_by = $4, decided_at = $5 WHERE number = $6`,
			SessionFilingAccepted, link.ID, projectNumber, decidedBy, now, filingNumber)
		return err
	})
	if err != nil {
		return SessionFiling{}, Link{}, err
	}
	filing, err := store.SessionFiling(id)
	return filing, link, err
}

// DismissSessionFiling records that a person said to leave the session
// unfiled. A filing already decided is a conflict.
func (store *Store) DismissSessionFiling(id, decidedBy string) (SessionFiling, error) {
	number, ok := recordNumberOf(SessionFilingIDPrefix, id)
	if !ok {
		return SessionFiling{}, fmt.Errorf("session filing %s: %w", id, ErrNotFound)
	}
	result, err := store.database.Exec(`UPDATE session_filings SET status = $1, decided_by = $2, decided_at = $3 WHERE number = $4 AND status = $5`,
		SessionFilingDismissed, decidedBy, store.now().Unix(), number, SessionFilingProposed)
	if err != nil {
		return SessionFiling{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return SessionFiling{}, err
	} else if affected == 0 {
		current, err := store.SessionFiling(id)
		if err != nil {
			return SessionFiling{}, err
		}
		return SessionFiling{}, fmt.Errorf("%w: session filing %s is already %s", ErrConflict, id, current.Status)
	}
	return store.SessionFiling(id)
}

// SupersedeProposedSessionFilings closes a session's undecided proposals
// because someone filed the session directly.
func (store *Store) SupersedeProposedSessionFilings(sessionID, decidedBy string) error {
	_, err := store.database.Exec(`UPDATE session_filings SET status = $1, decided_by = $2, decided_at = $3 WHERE session_id = $4 AND status = $5`,
		SessionFilingSuperseded, decidedBy, store.now().Unix(), sessionID, SessionFilingProposed)
	return err
}

func (handlers *Handlers) ListSessionFilings(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	for name := range query {
		if !contains([]string{"status", "session_id", "project_id", "limit"}, name) {
			writeError(writer, fmt.Errorf("%w: unknown parameter %q; this route takes status, session_id, project_id and limit", ErrInvalid, name))
			return
		}
	}
	filter := SessionFilingFilter{Status: query.Get("status"), SessionID: query.Get("session_id"), ProjectID: query.Get("project_id")}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			writeError(writer, fmt.Errorf("%w: limit must be a positive whole number", ErrInvalid))
			return
		}
		filter.Limit = limit
	}
	filings, err := handlers.Store.SessionFilings(filter)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session_filings": filings})
}

func (handlers *Handlers) AcceptSessionFiling(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "decided_by", "project_id")
	if err != nil {
		writeError(writer, err)
		return
	}
	var decidedBy, projectID string
	for name, target := range map[string]*string{"decided_by": &decidedBy, "project_id": &projectID} {
		value, err := stringField(fields, name)
		if err != nil {
			writeError(writer, err)
			return
		}
		if value != nil {
			*target = strings.TrimSpace(*value)
		}
	}
	filing, err := handlers.Store.SessionFiling(request.PathValue("session_filing_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	if filing.Status != SessionFilingProposed {
		writeError(writer, fmt.Errorf("%w: session filing %s is already %s", ErrConflict, filing.ID, filing.Status))
		return
	}
	if projectID == "" {
		projectID = filing.ProjectID
	}
	if projectID == "" {
		writeError(writer, fmt.Errorf("%w: session filing %s proposes no project, so project_id is required", ErrInvalid, filing.ID))
		return
	}
	// Every link is checked with its owner before it is written.
	label, err := handlers.Owners.Label(request.Context(), EntitySession, filing.SessionID)
	if err != nil {
		writeError(writer, err)
		return
	}
	accepted, link, err := handlers.Store.AcceptSessionFiling(filing.ID, projectID, decidedBy, label)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session_filing": accepted, "link": link})
}

func (handlers *Handlers) DismissSessionFiling(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "decided_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	decidedBy, err := stringField(fields, "decided_by")
	if err != nil {
		writeError(writer, err)
		return
	}
	if decidedBy == nil {
		empty := ""
		decidedBy = &empty
	}
	dismissed, err := handlers.Store.DismissSessionFiling(request.PathValue("session_filing_id"), strings.TrimSpace(*decidedBy))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, dismissed)
}

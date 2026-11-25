package rulestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SubjectTypeSession is the subject type of SourceSession.
const SubjectTypeSession = "session"

// Subject is rule-store's latest copy of one subject, and its state history
// as far as rule-store saw it.
type Subject struct {
	Type            string          `json:"subject_type"`
	ID              string          `json:"subject_id"`
	State           string          `json:"state"`
	StateEpisode    int64           `json:"state_episode"`
	StateObservedAt int64           `json:"state_observed_at"`
	Snapshot        json.RawMessage `json:"snapshot"`
	ObservedAt      int64           `json:"observed_at"`
}

// TriggerKey is what a run is unique on besides its revision and subject: the
// state and which time the subject entered it. A rule fires at most once each
// time a subject enters a state.
func (s Subject) TriggerKey() string { return fmt.Sprintf("%s#%d", s.State, s.StateEpisode) }

// SessionSnapshot is the part of an llm-bridge-server session a condition can
// test. The snapshot stays verbatim in the subjects table; this is a reading
// of it.
type SessionSnapshot struct {
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	Purpose    string `json:"purpose"`
	FolderName string `json:"folder_name"`
	Harness    string `json:"harness"`
	Origin     string `json:"origin"`
	Type       string `json:"type"`
	UpdatedAt  string `json:"updated_at"`
	Status     struct {
		Reason string `json:"reason"`
	} `json:"status"`
}

// Field returns one of SessionFields.
func (s SessionSnapshot) Field(name string) (string, error) {
	switch name {
	case "state":
		return s.State, nil
	case "purpose":
		return s.Purpose, nil
	case "folder_name":
		return s.FolderName, nil
	case "harness":
		return s.Harness, nil
	case "origin":
		return s.Origin, nil
	case "type":
		return s.Type, nil
	case "status_reason":
		return s.Status.Reason, nil
	}
	return "", fmt.Errorf("session field %q is not one rule-store reads", name)
}

// ParseSessionSnapshot reads a session as llm-bridge-server sends it. A
// session without an id or a state is refused: rule-store keys on the one and
// counts changes of the other, and guessing either would decide wrongly.
func ParseSessionSnapshot(raw json.RawMessage) (SessionSnapshot, error) {
	var s SessionSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("read session: %w", err)
	}
	if strings.TrimSpace(s.SessionID) == "" || strings.TrimSpace(s.State) == "" {
		return s, fmt.Errorf("session without session_id or state: %.200s", raw)
	}
	return s, nil
}

// SubjectObservation is one copy of a subject to record.
type SubjectObservation struct {
	Type     string
	ID       string
	State    string
	Snapshot json.RawMessage
}

// ObserveSubjects records copies in one transaction and returns each subject
// as now stored. A changed state starts a new episode; an unchanged one keeps
// it, so a frame that only bumps updated_at cannot fire a rule a second time.
func (s *Store) ObserveSubjects(observations []SubjectObservation) ([]Subject, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	at := now()
	stored := make([]Subject, 0, len(observations))
	for _, o := range observations {
		var state string
		var episode, stateObservedAt int64
		err := tx.QueryRow(`SELECT state, state_episode, state_observed_at FROM subjects WHERE subject_type = ? AND subject_id = ?`,
			o.Type, o.ID).Scan(&state, &episode, &stateObservedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			episode, stateObservedAt = 1, at
		case err != nil:
			return nil, err
		case state != o.State:
			episode, stateObservedAt = episode+1, at
		}
		if _, err := tx.Exec(`INSERT INTO subjects (subject_type, subject_id, state, state_episode, state_observed_at, snapshot, observed_at)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT (subject_type, subject_id) DO UPDATE SET state = excluded.state, state_episode = excluded.state_episode,
				state_observed_at = excluded.state_observed_at, snapshot = excluded.snapshot, observed_at = excluded.observed_at`,
			o.Type, o.ID, o.State, episode, stateObservedAt, string(o.Snapshot), at); err != nil {
			return nil, err
		}
		stored = append(stored, Subject{Type: o.Type, ID: o.ID, State: o.State, StateEpisode: episode,
			StateObservedAt: stateObservedAt, Snapshot: o.Snapshot, ObservedAt: at})
	}
	return stored, tx.Commit()
}

// ForgetSubject drops the copy of a subject its source deleted. Runs about it
// stay.
func (s *Store) ForgetSubject(subjectType, subjectID string) error {
	_, err := s.db.Exec(`DELETE FROM subjects WHERE subject_type = ? AND subject_id = ?`, subjectType, subjectID)
	return err
}

// GetSubject returns rule-store's copy of one subject.
func (s *Store) GetSubject(subjectType, subjectID string) (*Subject, error) {
	subjects, err := s.querySubjects(`WHERE subject_type = ? AND subject_id = ?`, subjectType, subjectID)
	if err != nil {
		return nil, err
	}
	if len(subjects) == 0 {
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, subjectType, subjectID)
	}
	return &subjects[0], nil
}

// SubjectsInStates returns every subject of a type in one of states, or in
// any state when states is empty.
func (s *Store) SubjectsInStates(subjectType string, states []string) ([]Subject, error) {
	if len(states) == 0 {
		return s.querySubjects(`WHERE subject_type = ?`, subjectType)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := []any{subjectType}
	for _, state := range states {
		args = append(args, state)
	}
	return s.querySubjects(`WHERE subject_type = ? AND state IN (`+placeholders+`)`, args...)
}

func (s *Store) querySubjects(where string, args ...any) ([]Subject, error) {
	rows, err := s.db.Query(`SELECT subject_type, subject_id, state, state_episode, state_observed_at, snapshot, observed_at FROM subjects `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	subjects := []Subject{}
	for rows.Next() {
		var subject Subject
		var snapshot string
		if err := rows.Scan(&subject.Type, &subject.ID, &subject.State, &subject.StateEpisode, &subject.StateObservedAt, &snapshot, &subject.ObservedAt); err != nil {
			return nil, err
		}
		subject.Snapshot = json.RawMessage(snapshot)
		subjects = append(subjects, subject)
	}
	return subjects, rows.Err()
}

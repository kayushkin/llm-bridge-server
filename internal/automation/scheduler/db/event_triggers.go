package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
)

// EncodeEventTrigger writes a trigger as the JSON the event_trigger column
// holds; nil is "".
func EncodeEventTrigger(trigger *eventtriggers.Trigger) (string, error) {
	if trigger == nil {
		return "", nil
	}
	encoded, err := json.Marshal(trigger)
	if err != nil {
		return "", fmt.Errorf("encode event_trigger: %w", err)
	}
	return string(encoded), nil
}

// DecodeEventTrigger reads the event_trigger column, or a request's
// event_trigger field: "" and "null" are no trigger. A field the trigger type
// does not have is an error, so a misspelled "condition" is refused rather
// than stored as a trigger that matches every message.
func DecodeEventTrigger(encoded string) (*eventtriggers.Trigger, error) {
	if encoded == "" || encoded == "null" {
		return nil, nil
	}
	var trigger eventtriggers.Trigger
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&trigger); err != nil {
		return nil, fmt.Errorf("event_trigger: %w", err)
	}
	return &trigger, nil
}

// ErrEventAlreadyDelivered means the job already ran for this message.
var ErrEventAlreadyDelivered = errors.New("this message already started a run of this job")

// EventTriggerDelivered reports whether the message already started a run of
// the job.
func (s *Store) EventTriggerDelivered(jobID int64, idempotencyKey string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM event_trigger_deliveries WHERE job_id=$1 AND idempotency_key=$2`, jobID, idempotencyKey).Scan(&count)
	return count > 0, err
}

// RecordEventTriggerDelivery records that the message started this run.
func (s *Store) RecordEventTriggerDelivery(jobID int64, idempotencyKey string, runID int64) error {
	result, err := s.db.Exec(`INSERT INTO event_trigger_deliveries (job_id, idempotency_key, run_id, created_at) VALUES ($1,$2,$3,$4) ON CONFLICT (job_id, idempotency_key) DO NOTHING`,
		jobID, idempotencyKey, runID, storedNow())
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 0 {
		return ErrEventAlreadyDelivered
	}
	return nil
}

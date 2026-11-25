package api

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
)

// eventTriggerSubject is one subject a job's event_trigger may name, with the
// fields its conditions and its prompt template may use. An editor reads this
// rather than keeping its own copy of the field list.
type eventTriggerSubject struct {
	Subject          string                `json:"subject"`
	Description      string                `json:"description"`
	IdempotencyField string                `json:"idempotency_field"`
	Fields           []eventtriggers.Field `json:"fields"`
}

func (s *Server) handleEventTriggerSubjects(w http.ResponseWriter, r *http.Request) {
	subjects := make([]eventTriggerSubject, 0, len(eventtriggers.Subjects))
	for _, subject := range eventtriggers.Subjects {
		subjects = append(subjects, eventTriggerSubject{
			Subject:          subject.Name,
			Description:      subject.Description,
			IdempotencyField: subject.IdempotencyField,
			Fields:           subject.Fields(),
		})
	}
	writeJSON(w, subjects)
}

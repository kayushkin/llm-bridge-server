package eventtriggerconsumer

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/cron"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
)

type fakeStore struct {
	jobs      []db.Job
	listErr   error
	delivered map[int64]map[string]int64
}

func (s *fakeStore) ListJobs() ([]db.Job, error) { return s.jobs, s.listErr }
func (s *fakeStore) EventTriggerDelivered(jobID int64, key string) (bool, error) {
	_, found := s.delivered[jobID][key]
	return found, nil
}
func (s *fakeStore) RecordEventTriggerDelivery(jobID int64, key string, runID int64) error {
	if s.delivered[jobID] == nil {
		s.delivered[jobID] = map[string]int64{}
	}
	s.delivered[jobID][key] = runID
	return nil
}

type fakeStarter struct {
	outcomes map[int64]cron.TriggeredRunOutcome
	err      error
	started  []int64
	nextRun  int64
}

func (s *fakeStarter) StartTriggeredRun(jobID int64, message eventtriggers.Message) (cron.TriggeredRunOutcome, int64, error) {
	if s.err != nil {
		return 0, 0, s.err
	}
	outcome := s.outcomes[jobID]
	if outcome != cron.TriggeredRunStarted {
		return outcome, 0, nil
	}
	s.started = append(s.started, jobID)
	s.nextRun++
	return outcome, s.nextRun, nil
}

func inboundSubject(t *testing.T) eventtriggers.Subject {
	subject, known := eventtriggers.FindSubject(messages.MessagingInboundSubject)
	if !known {
		t.Fatal("messaging.inbound unknown")
	}
	return subject
}

func directMessage(t *testing.T, eventID, platform string) []byte {
	data, err := json.Marshal(messages.MessagingInbound{EventID: eventID, Platform: platform, IsDirectMessage: true, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func triggeredJob(id int64, platform string) db.Job {
	return db.Job{ID: id, Name: "j", Type: "agent", Enabled: true, Prompt: "{{.body}}", EventTrigger: &eventtriggers.Trigger{
		Subject:    messages.MessagingInboundSubject,
		Conditions: []eventtriggers.Condition{{Field: "platform", Equals: json.RawMessage(`"` + platform + `"`)}},
	}}
}

func newConsumer(jobs ...db.Job) (*Consumer, *fakeStore, *fakeStarter) {
	store := &fakeStore{jobs: jobs, delivered: map[int64]map[string]int64{}}
	starter := &fakeStarter{outcomes: map[int64]cron.TriggeredRunOutcome{}}
	return &Consumer{Store: store, Starter: starter}, store, starter
}

func TestAMessageStartsOnlyTheJobsItMatches(t *testing.T) {
	disabled := triggeredJob(3, "discord")
	disabled.Enabled = false
	scheduled := db.Job{ID: 4, Name: "cron", Type: "agent", Enabled: true, Schedule: "* * * * *"}
	consumer, store, starter := newConsumer(triggeredJob(1, "discord"), triggeredJob(2, "telegram"), disabled, scheduled)

	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Acknowledge {
		t.Fatalf("disposition = %v, want Acknowledge", got)
	}
	if len(starter.started) != 1 || starter.started[0] != 1 {
		t.Fatalf("started %v, want only job 1", starter.started)
	}
	if store.delivered[1]["$a"] != 1 {
		t.Fatalf("delivery of $a to job 1 not recorded: %v", store.delivered)
	}
}

func TestARedeliveredMessageStartsNothingTwice(t *testing.T) {
	consumer, _, starter := newConsumer(triggeredJob(1, "discord"))
	consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord"))
	consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord"))
	if len(starter.started) != 1 {
		t.Fatalf("started %v, want one run", starter.started)
	}
}

// A busy job asks for the message again; the job that did start is recorded,
// so the redelivery starts only the busy one.
func TestABusyJobAsksForTheMessageAgain(t *testing.T) {
	consumer, _, starter := newConsumer(triggeredJob(1, "discord"), triggeredJob(2, "discord"))
	starter.outcomes[2] = cron.TriggeredRunJobBusy
	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Redeliver {
		t.Fatalf("disposition = %v, want Redeliver", got)
	}
	starter.outcomes[2] = cron.TriggeredRunStarted
	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Acknowledge {
		t.Fatalf("second disposition = %v, want Acknowledge", got)
	}
	if len(starter.started) != 2 || starter.started[0] != 1 || starter.started[1] != 2 {
		t.Fatalf("started %v, want job 1 then job 2", starter.started)
	}
}

func TestAJobThatCannotRunAnyMoreDropsTheMessage(t *testing.T) {
	consumer, _, starter := newConsumer(triggeredJob(1, "discord"))
	starter.outcomes[1] = cron.TriggeredRunNotRunnable
	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Acknowledge {
		t.Fatalf("disposition = %v, want Acknowledge", got)
	}
}

func TestFailuresAskForTheMessageAgain(t *testing.T) {
	consumer, store, _ := newConsumer(triggeredJob(1, "discord"))
	store.listErr = errors.New("database is locked")
	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Redeliver {
		t.Fatalf("list failure: disposition = %v, want Redeliver", got)
	}
	consumer, _, starter := newConsumer(triggeredJob(1, "discord"))
	starter.err = errors.New("create run: disk full")
	if got := consumer.Handle(inboundSubject(t), directMessage(t, "$a", "discord")); got != Redeliver {
		t.Fatalf("start failure: disposition = %v, want Redeliver", got)
	}
}

func TestAnUndecodableMessageIsDiscarded(t *testing.T) {
	consumer, _, _ := newConsumer(triggeredJob(1, "discord"))
	if got := consumer.Handle(inboundSubject(t), []byte(`{"is_direct_message":"yes"}`)); got != Discard {
		t.Fatalf("disposition = %v, want Discard", got)
	}
}

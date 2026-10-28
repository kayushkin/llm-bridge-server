// Package eventtriggerconsumer reads the bus subjects jobs can be triggered by
// and starts a run of every job whose event_trigger a message matches.
package eventtriggerconsumer

import (
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/eventtriggers"
)

// RetryDelay is how long a message waits before JetStream delivers it again,
// when a job it matched was still running or the database could not be read.
const RetryDelay = 30 * time.Second

// Store is what the consumer reads and writes in the scheduler's database.
type Store interface {
	ListJobs() ([]db.Job, error)
	EventTriggerDelivered(jobID int64, idempotencyKey string) (bool, error)
	RecordEventTriggerDelivery(jobID int64, idempotencyKey string, runID int64) error
}

// RunStarter starts one run for one message; cron.Scheduler is one.
type RunStarter interface {
	StartTriggeredRun(jobID int64, message eventtriggers.Message) (cron.TriggeredRunOutcome, int64, error)
}

// Disposition is what to tell JetStream about a message.
type Disposition int

const (
	// Acknowledge: every matching job ran or will never run for it.
	Acknowledge Disposition = iota
	// Redeliver: a matching job is still running, or the database failed;
	// ask for the message again after RetryDelay. Jobs it already started
	// are recorded, so the next delivery does not start them twice.
	Redeliver
	// Discard: the message does not decode, and no later delivery will.
	Discard
)

// Consumer matches messages against triggered jobs.
type Consumer struct {
	Store   Store
	Starter RunStarter
}

// Handle decides what one message starts and what to tell JetStream.
func (c *Consumer) Handle(subject eventtriggers.Subject, data []byte) Disposition {
	message, err := eventtriggers.DecodeMessage(subject, data)
	if err != nil {
		log.Printf("event triggers: discarding a message on %s: %v", subject.Name, err)
		return Discard
	}
	jobs, err := c.Store.ListJobs()
	if err != nil {
		log.Printf("event triggers: %s message %s: list jobs: %v", subject.Name, message.IdempotencyKey(), err)
		return Redeliver
	}
	key := message.IdempotencyKey()
	disposition := Acknowledge
	for _, job := range jobs {
		if !job.Enabled || job.EventTrigger == nil || job.EventTrigger.Subject != subject.Name {
			continue
		}
		matched, err := eventtriggers.Matches(*job.EventTrigger, message)
		if err != nil {
			// Validate refuses such a trigger when it is saved; one that got
			// through anyway must not stop the other jobs.
			log.Printf("event triggers: job %d (%s): %v", job.ID, job.Name, err)
			continue
		}
		if !matched {
			continue
		}
		delivered, err := c.Store.EventTriggerDelivered(job.ID, key)
		if err != nil {
			log.Printf("event triggers: job %d (%s), message %s: %v", job.ID, job.Name, key, err)
			disposition = Redeliver
			continue
		}
		if delivered {
			continue
		}
		outcome, runID, err := c.Starter.StartTriggeredRun(job.ID, message)
		if err != nil {
			log.Printf("event triggers: job %d (%s), message %s: %v", job.ID, job.Name, key, err)
			disposition = Redeliver
			continue
		}
		switch outcome {
		case cron.TriggeredRunJobBusy:
			disposition = Redeliver
		case cron.TriggeredRunStarted:
			if err := c.Store.RecordEventTriggerDelivery(job.ID, key, runID); err != nil {
				// The run is going. Unrecorded, a redelivery of this message
				// would start the job again, and nothing else would say why.
				log.Printf("event triggers: job %d (%s) run %d for message %s started but was not recorded: %v", job.ID, job.Name, runID, key, err)
			}
		}
	}
	return disposition
}

// JetStream is the part of the bus client the consumer subscribes with.
type JetStream interface {
	JetConsumeNew(subject, durable string, handler nats.MsgHandler) (*nats.Subscription, error)
}

// Subscribe starts one durable consumer for every subject in
// eventtriggers.Subjects. A new consumer begins at messages published after
// it, so turning the feature on does not replay the stream's history.
func (c *Consumer) Subscribe(bus JetStream) error {
	for _, subject := range eventtriggers.Subjects {
		subject := subject
		_, err := bus.JetConsumeNew(subject.Name, subject.ConsumerName(), func(msg *nats.Msg) {
			var err error
			switch c.Handle(subject, msg.Data) {
			case Acknowledge:
				err = msg.Ack()
			case Redeliver:
				err = msg.NakWithDelay(RetryDelay)
			case Discard:
				err = msg.Term()
			}
			if err != nil {
				log.Printf("event triggers: answering JetStream for a %s message: %v", subject.Name, err)
			}
		})
		if err != nil {
			return fmt.Errorf("consume %s as %s: %w", subject.Name, subject.ConsumerName(), err)
		}
		log.Printf("event triggers: consuming %s as %s", subject.Name, subject.ConsumerName())
	}
	return nil
}

package logging

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	natsbus "github.com/kayushkin/bus"

	"github.com/slava/scheduler/internal/runstatus"
)

type LogEntry struct {
	Source   string                 `json:"source"`
	Level    string                 `json:"level"`
	Type     string                 `json:"type"`
	Content  string                 `json:"content"`
	Time     string                 `json:"time"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// outputSnippetBudget is how much of a job's combined output travels with the
// finish event. The snippet is stored by whatever reads the bus, so the cut is
// a wire value rather than a display one.
const outputSnippetBudget = 500

// truncationMarker is appended to a snippet that did not fit.
const truncationMarker = "... (truncated)"

// EntryPublisher is the sink a Logger writes to. *natsbus.Client satisfies it.
// It is an interface so that a test can read back the entry a Log* call
// produced; with a concrete client nothing could observe the level a finished
// run was filed at, which is how the timeout case went unnoticed.
type EntryPublisher interface {
	Publish(subject string, data any) error
}

type Logger struct {
	client EntryPublisher
}

func New(natsClient *natsbus.Client) *Logger {
	if natsClient == nil {
		// A nil *natsbus.Client in an interface field is not a nil interface,
		// so store an untyped nil and keep publish's guard meaningful.
		return &Logger{}
	}
	return &Logger{client: natsClient}
}

// NewPublishingTo builds a Logger over any sink. It exists so that a package
// holding a *Logger it did not build — cmd/scheduler hands one to the cron
// loop, the bus handler and the API — can read back what was published.
func NewPublishingTo(publisher EntryPublisher) *Logger {
	return &Logger{client: publisher}
}

func (l *Logger) publish(entry LogEntry) {
	if l.client == nil {
		return
	}
	if err := l.client.Publish("logs.scheduler", entry); err != nil {
		b, _ := json.Marshal(entry)
		log.Printf("logging: failed to publish to NATS: %v (entry: %s)", err, b)
	}
}

// truncateAtRuneBoundary cuts s to at most budget bytes, walking the cut back
// to the start of the rune that straddles it. Cutting at a fixed byte offset
// splits that rune, and nothing downstream reports it: json.Marshal substitutes
// U+FFFD for the partial sequence and returns no error, so the damage is stored
// and never announced.
//
// It never drops more than the width of the straddling rune, so the snippet
// stays within one rune of its budget.
func truncateAtRuneBoundary(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// LogJobStart logs job execution start
func (l *Logger) LogJobStart(jobID int64, jobName, schedule string, runID int64) {
	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   "info",
		Type:    "job_execution",
		Content: fmt.Sprintf("Job '%s' started execution", jobName),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"job_id":   jobID,
			"job_name": jobName,
			"schedule": schedule,
			"run_id":   runID,
			"action":   "start",
		},
	})
}

// LogJobFinish logs job execution completion
func (l *Logger) LogJobFinish(jobID int64, jobName, schedule string, runID int64, status, output string, duration time.Duration) {
	level := "info"
	if runstatus.TerminalStatusIsFailure(status) {
		level = "error"
	}

	outputSnippet := output
	if len(output) > outputSnippetBudget {
		outputSnippet = truncateAtRuneBoundary(output, outputSnippetBudget) + truncationMarker
	}

	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   level,
		Type:    "job_execution",
		Content: fmt.Sprintf("Job '%s' finished with status: %s", jobName, status),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"job_id":         jobID,
			"job_name":       jobName,
			"schedule":       schedule,
			"run_id":         runID,
			"action":         "finish",
			"status":         status,
			"duration_ms":    duration.Milliseconds(),
			"output_snippet": outputSnippet,
		},
	})
}

// LogJobCreated logs job creation
func (l *Logger) LogJobCreated(jobID int64, jobName, schedule, command string) {
	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   "info",
		Type:    "job_crud",
		Content: fmt.Sprintf("Job '%s' created", jobName),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"job_id":   jobID,
			"job_name": jobName,
			"schedule": schedule,
			"command":  command,
			"action":   "create",
		},
	})
}

// LogJobUpdated logs job updates
func (l *Logger) LogJobUpdated(jobID int64, jobName string, field string, value interface{}) {
	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   "info",
		Type:    "job_crud",
		Content: fmt.Sprintf("Job '%s' updated: %s = %v", jobName, field, value),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"job_id":   jobID,
			"job_name": jobName,
			"action":   "update",
			"field":    field,
			"value":    value,
		},
	})
}

// LogJobDeleted logs job deletion
func (l *Logger) LogJobDeleted(jobID int64, jobName string) {
	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   "info",
		Type:    "job_crud",
		Content: fmt.Sprintf("Job '%s' deleted", jobName),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"job_id":   jobID,
			"job_name": jobName,
			"action":   "delete",
		},
	})
}

// LogSchedulerLifecycle logs scheduler start/stop/reload events
func (l *Logger) LogSchedulerLifecycle(action string, details ...string) {
	content := fmt.Sprintf("Scheduler %s", action)
	if len(details) > 0 {
		content = fmt.Sprintf("Scheduler %s: %s", action, strings.Join(details, ", "))
	}

	l.publish(LogEntry{
		Source:  "scheduler",
		Level:   "info",
		Type:    "lifecycle",
		Content: content,
		Time:    time.Now().UTC().Format(time.RFC3339),
		Metadata: map[string]interface{}{
			"action": action,
		},
	})
}

// LogError logs errors
func (l *Logger) LogError(errorType, message string, metadata map[string]interface{}) {
	if metadata == nil {
		metadata = make(map[string]interface{})
	}
	metadata["error_type"] = errorType

	l.publish(LogEntry{
		Source:   "scheduler",
		Level:    "error",
		Type:     "error",
		Content:  message,
		Time:     time.Now().UTC().Format(time.RFC3339),
		Metadata: metadata,
	})
}

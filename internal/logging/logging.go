package logging

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

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

// entryPublisher is the sink a Logger writes to. *natsbus.Client satisfies it.
// It is an interface so that a test can read back the entry a Log* call
// produced; with a concrete client nothing in this package could observe the
// level a finished run was filed at, which is how the timeout case went
// unnoticed.
type entryPublisher interface {
	Publish(subject string, data any) error
}

type Logger struct {
	client entryPublisher
}

func New(natsClient *natsbus.Client) *Logger {
	if natsClient == nil {
		// A nil *natsbus.Client in an interface field is not a nil interface,
		// so store an untyped nil and keep publish's guard meaningful.
		return &Logger{}
	}
	return &Logger{client: natsClient}
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
	if len(output) > 500 {
		outputSnippet = output[:500] + "... (truncated)"
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

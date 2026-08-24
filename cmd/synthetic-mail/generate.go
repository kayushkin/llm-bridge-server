package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/anthropic"
)

// messagesPerModelCall bounds one batch.
//
// Eight rather than the twenty-five that seemed reasonable at first, for the
// reason email-classifier learned the hard way: reply size is what makes a
// structured answer come back malformed, and the batch is the knob that
// controls reply size. Each message here carries a subject and several
// paragraphs, so the replies are considerably larger than a classification.
const messagesPerModelCall = 8

// maxOutputTokens is sized for a full batch of bodies with room to spare. Too
// small does not fail loudly by itself — a truncated tool call still decodes —
// which is why anthropic.InvokeTool checks stop_reason before the content.
const maxOutputTokens = 8192

const writeToolName = "write_messages"

const systemPrompt = `You write the bodies of emails for a synthetic mailbox used to test an email
classifier and a work-tracking board.

The mailbox belongs to a software engineer at a company called Northwind, which
runs a product-catalogue API. His colleagues write to him about work.

Rules:

1. Write what a real colleague would send. Real mail is uneven — some messages
   are two lines, some ramble, some are blunt. Do not make every message the
   same length or the same politeness.
2. The body must be about the specific work item given. A body that would fit
   any task teaches the classifier nothing.
3. Match the sender's role and the message kind you are told. A build robot does
   not write greetings. A vendor support desk does not sound like a colleague.
4. Plain text only. No markdown, no HTML, no bullet characters other than "-".
   Line breaks are real newlines.
5. Never invent a date, a deadline or a name that contradicts what you are given.
   Refer to timing in relative terms ("before the release", "by Friday") unless
   a date is supplied.
6. Do not sign off as anyone other than the named sender, and do not write the
   recipient's reply.
7. A reply must actually answer the message it is replying to, and must not
   restate it. Its subject should be the parent's subject prefixed with "Re: ".

Return one entry per message you are given, using the same key.`

// writeToolSchema is the forced tool. Every field is required, because a
// message missing its subject or body is not a partial success — it is a gap in
// the corpus that would have to be found later by reading it.
func writeToolSchema() map[string]any {
	return map[string]any{
		"name":        writeToolName,
		"description": "Return the written subject and body for every message requested.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"key":     map[string]any{"type": "string", "description": "The key of the message being written, copied exactly."},
							"subject": map[string]any{"type": "string", "description": "The Subject header. No 'Re:' unless this is a reply."},
							"body":    map[string]any{"type": "string", "description": "The plain-text body, with real newlines."},
						},
						"required": []string{"key", "subject", "body"},
					},
				},
			},
			"required": []string{"messages"},
		},
	}
}

type writtenMessage struct {
	Key     string `json:"key"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type writeReport struct {
	Messages []writtenMessage `json:"messages"`
}

// generate fills in the subject and body of every message that has none.
//
// Messages already carrying prose are left alone, so a run that failed part way
// can be repeated without paying to rewrite what already succeeded and without
// changing mail that has already been appended.
func generate(ctx context.Context, cfg anthropic.Config, messages []Message, itemsByTitle map[string]workItem) error {
	pending := make([]int, 0, len(messages))
	for i := range messages {
		if !messages[i].Generated() {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		fmt.Println("  every message already has prose; nothing to generate")
		return nil
	}

	for start := 0; start < len(pending); start += messagesPerModelCall {
		end := start + messagesPerModelCall
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		fmt.Printf("  writing %d message(s) [%d/%d]…\n", len(batch), end, len(pending))
		if err := generateBatch(ctx, cfg, messages, batch, itemsByTitle); err != nil {
			return fmt.Errorf("batch starting at %s: %w", messages[batch[0]].Key, err)
		}
	}
	return nil
}

func generateBatch(ctx context.Context, cfg anthropic.Config, messages []Message, batch []int, itemsByTitle map[string]workItem) error {
	var report writeReport
	if err := anthropic.InvokeTool(ctx, cfg, systemPrompt, writeToolSchema(), writeToolName,
		buildBrief(messages, batch, itemsByTitle), &report); err != nil {
		return err
	}

	written := make(map[string]writtenMessage, len(report.Messages))
	for _, w := range report.Messages {
		written[w.Key] = w
	}

	// Every message asked for must come back. A missing one would otherwise sit
	// in the corpus with an empty body and only be noticed when somebody opened
	// it in the drawer.
	var missing []string
	for _, i := range batch {
		w, ok := written[messages[i].Key]
		if !ok || strings.TrimSpace(w.Subject) == "" || strings.TrimSpace(w.Body) == "" {
			missing = append(missing, messages[i].Key)
			continue
		}
		messages[i].Subject = strings.TrimSpace(w.Subject)
		messages[i].Body = strings.TrimSpace(w.Body)
	}
	if len(missing) > 0 {
		return fmt.Errorf("the model returned nothing usable for %s", strings.Join(missing, ", "))
	}
	return nil
}

// buildBrief describes one batch to the model.
//
// A reply is given its parent's subject and body in full. Without them the model
// writes a reply to a message it has never read, which produces prose that
// mentions nothing the parent said — and threading is one of the things this
// corpus exists to exercise.
func buildBrief(messages []Message, batch []int, itemsByTitle map[string]workItem) string {
	byKey := make(map[string]*Message, len(messages))
	for i := range messages {
		byKey[messages[i].Key] = &messages[i]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Today is %s.\n\n", time.Now().UTC().Format("Monday 2 January 2006"))
	fmt.Fprintf(&b, "Write %d message(s).\n\n", len(batch))

	for _, i := range batch {
		m := messages[i]
		spec := specFor(m.Kind)
		fmt.Fprintf(&b, "--- key: %s\n", m.Key)
		fmt.Fprintf(&b, "kind: %s\n", m.Kind)
		fmt.Fprintf(&b, "from: %s <%s> — %s\n", m.From.Name, m.From.Email, m.From.Role)
		fmt.Fprintf(&b, "to: %s <%s> — %s\n", owner.Name, owner.Email, owner.Role)
		fmt.Fprintf(&b, "sent: %s\n", m.SentAt.Format("Monday 2 January, 15:04"))
		fmt.Fprintf(&b, "what to write: %s\n", spec.Guidance)

		if m.WorkItem != "" {
			fmt.Fprintf(&b, "work item this concerns: %s\n", m.WorkItem)
			if item, ok := itemsByTitle[m.WorkItem]; ok && item.Body != "" {
				fmt.Fprintf(&b, "what that work item says: %s\n", collapse(item.Body, 600))
			}
		} else {
			b.WriteString("work item: none — this message is not about anything on the board\n")
		}

		if m.ReplyToKey != "" {
			if parent, ok := byKey[m.ReplyToKey]; ok {
				fmt.Fprintf(&b, "this is a REPLY to key %s, whose subject was: %s\n", parent.Key, parent.Subject)
				fmt.Fprintf(&b, "and whose body was:\n%s\n", collapse(parent.Body, 900))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// collapse trims text down to a budget without cutting mid-word.
func collapse(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) <= limit {
		return flat
	}
	cut := flat[:limit]
	if space := strings.LastIndex(cut, " "); space > limit/2 {
		cut = cut[:space]
	}
	return cut + "…"
}

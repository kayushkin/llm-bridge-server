package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mailstackClient appends generated mail to a local account.
//
// Only local accounts accept an append — mailstack refuses the route for
// google, microsoft and imap, because appending to a real provider would mean
// either sending mail or forging a message into somebody's real mailbox. That
// refusal is why this tool cannot damage a real account by being pointed at the
// wrong one.
type mailstackClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func newMailstackClient(baseURL, token string) *mailstackClient {
	return &mailstackClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type appendRequest struct {
	Subject    string    `json:"subject"`
	From       address   `json:"from"`
	To         []address `json:"to"`
	Date       time.Time `json:"date"`
	BodyText   string    `json:"body_text"`
	Unread     *bool     `json:"unread"`
	MessageID  string    `json:"message_id,omitempty"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
}

type address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type appendResponse struct {
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
	Locator string `json:"locator"`
}

// Append stores one message and returns its locator.
func (c *mailstackClient) Append(ctx context.Context, accountID string, m Message) (string, error) {
	to := make([]address, 0, len(m.To))
	for _, p := range m.To {
		to = append(to, address{Name: p.Name, Email: p.Email})
	}
	unread := m.Unread
	body, err := json.Marshal(appendRequest{
		Subject:    m.Subject,
		From:       address{Name: m.From.Name, Email: m.From.Email},
		To:         to,
		Date:       m.SentAt,
		BodyText:   m.Body,
		Unread:     &unread,
		MessageID:  m.MessageID,
		InReplyTo:  m.InReplyTo,
		References: m.References,
	})
	if err != nil {
		return "", fmt.Errorf("encode message %s: %w", m.Key, err)
	}

	url := fmt.Sprintf("%s/api/accounts/%s/messages", c.BaseURL, accountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build append request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("mailstack unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read mailstack reply: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mailstack returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var decoded appendResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("decode mailstack reply: %w", err)
	}
	if decoded.Locator == "" {
		return "", fmt.Errorf("mailstack accepted message %s but returned no locator", m.Key)
	}
	return decoded.Locator, nil
}

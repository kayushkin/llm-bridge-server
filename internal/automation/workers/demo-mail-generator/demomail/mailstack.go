// Package demomail delivers generated mail into a local mailstack account,
// through llm-bridge-server's /mail/ (bridgestores.MailURL). Its requests go
// out through the default transport, which bridgeauth stamps with the binary's
// service principal token; mailstack's own bearer is retired.
//
// Local accounts only. mailstack refuses an append to google, microsoft or imap
// — appending to a real provider would mean either sending mail or forging a
// message into somebody's real mailbox — so pointing this at the wrong account
// fails loudly rather than doing something irreversible.
package demomail

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

// Address is one sender or recipient.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Message is one email as a sender would have written it.
//
// It carries no date. Mail is appended at the moment it is generated, because
// the whole point of a live simulation is that the timestamps are real: a card,
// its notes, its agent sessions and its mail all have to agree about when
// things happened, and the only way to guarantee that is to let them all be now.
type Message struct {
	Subject    string    `json:"subject"`
	From       Address   `json:"from"`
	To         []Address `json:"to,omitempty"`
	BodyText   string    `json:"body_text"`
	MessageID  string    `json:"message_id,omitempty"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
	Unread     *bool     `json:"unread,omitempty"`
}

// Client appends to one mailstack.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the mailstack at mailURL, the bridge's address
// followed by /mail.
func New(mailURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(mailURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Append stores one message and returns its locator — "account:message id",
// the ref every consumer on this host uses to point at a message.
func (c *Client) Append(ctx context.Context, accountID string, m Message) (string, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode message: %w", err)
	}
	url := fmt.Sprintf("%s/api/accounts/%s/messages", c.BaseURL, accountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("mailstack unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mailstack returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded struct {
		Locator string `json:"locator"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("decode mailstack reply: %w", err)
	}
	if decoded.Locator == "" {
		return "", fmt.Errorf("mailstack accepted the message but returned no locator")
	}
	return decoded.Locator, nil
}

// Purge permanently removes every message in an account.
//
// Used to clear mail that describes work the repository cannot support — a
// request to bump a Node image in a repository with no Node in it produces a
// card no agent can ever satisfy, and leaving it on the board wastes a real
// session every time it is picked up.
func (c *Client) Purge(ctx context.Context, accountID string) (int, error) {
	removed := 0
	for _, folder := range []string{"INBOX", "SENT", "ARCHIVE", "TRASH"} {
		for {
			ids, err := c.listIDs(ctx, accountID, folder)
			if err != nil {
				return removed, err
			}
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				if err := c.permDelete(ctx, accountID, id); err != nil {
					return removed, err
				}
				removed++
			}
		}
	}
	return removed, nil
}

func (c *Client) listIDs(ctx context.Context, accountID, folder string) ([]string, error) {
	url := fmt.Sprintf("%s/api/messages?account=%s&folder=%s&limit=100", c.BaseURL, accountID, folder)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list %s: %d %s", folder, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var page struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(page.Messages))
	for _, m := range page.Messages {
		out = append(out, m.ID)
	}
	return out, nil
}

func (c *Client) permDelete(ctx context.Context, accountID, messageID string) error {
	url := fmt.Sprintf("%s/api/messages/%s/perm-delete?account=%s", c.BaseURL, messageID, accountID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("perm-delete %s: %d %s", messageID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

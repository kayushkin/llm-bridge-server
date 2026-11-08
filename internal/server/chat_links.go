package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Chat links: a session talking with a person outside the bridge, through
// one of our bot accounts in multichat. See msg/chat_link.go.
//
// Inbound: multichat publishes what a person sends our bots on
// messaging.inbound; runChatLinkInbound keeps each message for the active
// link holding its conversation, pending. deliverChatLinkInbound sends a
// link's pending messages into its session, together, when the session is
// free, as a turn whose client_request_id is the delivery id. Outbound: when
// a turn with a chat link delivery id ends (onTurnEnd), its final reply is
// kept and sent to the person through multichat, as the link's account,
// retried until multichat takes it. A turn anyone else starts carries no
// such id and is never sent.

// chatLinkDeliveryIDPrefix opens the client_request_id of a turn a chat link
// started; nothing else mints one.
const chatLinkDeliveryIDPrefix = "chat-link-delivery:"

// chatLinkReportRequestIDPrefix opens the client_request_id of a report a
// chat link sends its session; its turn's reply goes nowhere.
const chatLinkReportRequestIDPrefix = "chat-link-report:"

// How chat links pace themselves.
const (
	chatLinkSweepInterval = 10 * time.Second
	// chatLinkTurnWithoutEndAfter is how long a delivered turn may sit with
	// its session free and no turn end seen before it is called failed: an
	// error-terminated turn reports no turn end.
	chatLinkTurnWithoutEndAfter = 2 * time.Minute
	chatLinkFirstRetry          = 30 * time.Second
	chatLinkLongestRetry        = 30 * time.Minute
	chatLinkSendAttempts        = 12
	chatLinksInboundConsumer    = "llm-bridge-server-chat-links"
	chatLinkMultichatProvider   = "multichat"
)

// chatLinks is the server's chat-link machinery; nil when it is switched off.
type chatLinks struct {
	multichatURL string
	// peopleStoreURL is asked who owns the account a message came from.
	peopleStoreURL string
	httpClient     *http.Client
	// token answers multichat's bearer.
	token func(ctx context.Context) (string, error)
	// kick wakes the sweep early, after a message arrives or a turn ends.
	kick chan struct{}
	// mu keeps one sweep at a time.
	mu sync.Mutex
	// now is the clock.
	now func() time.Time
	// sendIntoSession runs POST /sessions/{id}/send through the server's own
	// routes (serveInternally); a test replaces it.
	sendIntoSession func(sessionID string, request msg.SendMessageRequest) (int, []byte)
	// references reads reference-store's id patterns, once a while.
	references chatLinkReferencePatterns
}

var errChatLinksOff = errors.New("chat links are off: LLMBRIDGE_NATS_URL, LLMBRIDGE_MULTICHAT_URL and LLMBRIDGE_PEOPLE_STORE_URL must all be set")

func (c *chatLinks) wake() {
	if c == nil {
		return
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// chatLinkSessionIsFree says whether a session in state can take a new turn
// now: nothing is running in it and nothing waits on a person's answer.
func chatLinkSessionIsFree(state msg.SessionState) bool {
	if state.IsActive() {
		return false
	}
	switch state {
	case msg.SessionAwaitingPermission, msg.SessionWaitingApproval:
		return false
	}
	return true
}

// chatLinkInboundText is what the session is sent: each message labeled by
// who sent it, read by id from people-store. A message from the person whose
// principal the session runs as is the operator's own; any other is labeled
// as not the operator's, so the agent never takes it for them. The closing
// note says what the far end can see, since the reply reaches them as a plain
// platform message.
func chatLinkInboundText(link store.ChatLink, sessionPrincipalID, accountName string, messages []store.ChatLinkMessage) string {
	via := platformName(link.Platform)
	if accountName != "" {
		via += ", through the " + accountName + " bot"
	}
	var text strings.Builder
	operatorWroteAny, othersWrote := false, []string{}
	previousSender := ""
	for index, message := range messages {
		name, isOperator := chatLinkSenderName(link, sessionPrincipalID, message)
		label := name
		if isOperator {
			label = name + " (the operator)"
			operatorWroteAny = true
		} else if !slices.Contains(othersWrote, name) {
			othersWrote = append(othersWrote, name)
		}
		if index > 0 {
			text.WriteString("\n\n---\n\n")
		}
		if index == 0 || label != previousSender {
			fmt.Fprintf(&text, "%s wrote to you on %s:\n\n", label, via)
		}
		previousSender = label
		if message.Body != "" {
			text.WriteString(message.Body)
		}
		for index, attachment := range message.Attachments {
			if message.Body != "" || index > 0 {
				text.WriteString("\n")
			}
			fmt.Fprintf(&text, "[attached file: %s (%s, %d bytes) %s]", attachment.Filename, attachment.ContentType, attachment.SizeBytes, attachment.URL)
		}
	}
	text.WriteString("\n\n")
	switch {
	case operatorWroteAny && len(othersWrote) == 0:
		text.WriteString("This is the operator, writing from their own account. Treat it as their own message. ")
	case operatorWroteAny:
		fmt.Fprintf(&text, "The messages marked (the operator) are the operator's own; the rest are from %s, not the operator. ", strings.Join(othersWrote, " and "))
	default:
		fmt.Fprintf(&text, "This is from %s, not the operator. ", strings.Join(othersWrote, " and "))
	}
	platform := platformName(link.Platform)
	fmt.Fprintf(&text, "Your final reply in this turn is sent back on %s word for word, as one message, so write it to them. "+
		"They see only that text: no buttons, no files or images shared into this chat, nothing else from the bridge's chat page. "+
		"Send links instead, keep to plain %s markdown, and expect a reply over 2000 characters to arrive in parts. (Chat link %s.)",
		platform, platform, link.ID)
	return text.String()
}

// chatLinkSenderName answers the name to label a message with, and whether
// its sender is the person the session runs as. Only ids decide that: the
// sender's principal, read from people-store, against the session's.
func chatLinkSenderName(link store.ChatLink, sessionPrincipalID string, message store.ChatLinkMessage) (string, bool) {
	name := message.SenderPersonDisplayName
	if name == "" {
		name = link.PersonDisplayName
	}
	isOperator := sessionPrincipalID != "" && message.SenderPrincipalID == sessionPrincipalID
	return name, isOperator
}

func platformName(platform string) string {
	switch platform {
	case "discord":
		return "Discord"
	}
	return platform
}

// --- multichat ---

type multichatAccounts struct {
	BotAccounts []struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		DisplayName string `json:"display_name"`
	} `json:"bot_accounts"`
}

func (c *chatLinks) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	token, err := c.token(ctx)
	if err != nil {
		return 0, fmt.Errorf("multichat token from auth-store: %w", err)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.multichatURL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("multichat %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, fmt.Errorf("multichat %s %s: %w", method, path, err)
	}
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, fmt.Errorf("multichat %s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return response.StatusCode, fmt.Errorf("multichat %s %s: %w", method, path, err)
		}
	}
	return response.StatusCode, nil
}

// botAccountName answers the display name multichat gives a bot account on
// platform, or an error when multichat holds no such bot.
func (c *chatLinks) botAccountName(ctx context.Context, accountID, platform string) (string, error) {
	var accounts multichatAccounts
	if _, err := c.call(ctx, http.MethodGet, "/api/accounts", nil, &accounts); err != nil {
		return "", err
	}
	for _, account := range accounts.BotAccounts {
		if account.ID == accountID {
			if account.Platform != platform {
				return "", fmt.Errorf("multichat's account %s is on %s, not %s", accountID, account.Platform, platform)
			}
			return account.DisplayName, nil
		}
	}
	return "", fmt.Errorf("multichat holds no bot account %s (GET /api/accounts lists them)", accountID)
}

type multichatSendResult struct {
	MessageIDs []string `json:"message_ids"`
}

// sendIntoSessionThroughRoutes is chatLinks.sendIntoSession in production.
func (s *Server) sendIntoSessionThroughRoutes(sessionID string, request msg.SendMessageRequest) (int, []byte) {
	return s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/send", request)
}

// --- routes ---

func (s *Server) chatLinksOrRefuse(w http.ResponseWriter) bool {
	if s.chatLinks == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "chat_links_off", errChatLinksOff.Error())
		return false
	}
	return true
}

// handleCreateChatLink: POST /sessions/{id}/chat-links.
func (s *Server) handleCreateChatLink(w http.ResponseWriter, r *http.Request) {
	if !s.chatLinksOrRefuse(w) {
		return
	}
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	var create msg.ChatLinkCreate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&create); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_chat_link", "body is not a chat link: "+err.Error())
		return
	}
	for field, value := range map[string]string{"account_id": create.AccountID, "platform": create.Platform,
		"conversation_id": create.ConversationID, "person_display_name": create.PersonDisplayName} {
		if strings.TrimSpace(value) == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_chat_link", field+" is required")
			return
		}
	}
	if _, err := s.chatLinks.botAccountName(r.Context(), create.AccountID, create.Platform); err != nil {
		writeJSONError(w, http.StatusBadRequest, "unknown_account", err.Error())
		return
	}
	caller, _ := callerOfRequest(r)
	link := store.ChatLink{
		SessionID: session.SessionID, AccountID: create.AccountID, Platform: create.Platform,
		ConversationID: create.ConversationID, PersonDisplayName: create.PersonDisplayName,
		CreatedByPrincipalID: caller.principalID, CreatedBySessionAgent: caller.postingAgentOfSessionID != "",
		CreatedAt: s.chatLinks.now(),
	}
	if caller.postingAgentOfSessionID != "" {
		link.CreatedByPrincipalID = session.PrincipalID
	}
	created, err := s.store.CreateChatLink(link)
	if errors.Is(err, store.ErrChatLinkConversationTaken) {
		writeJSONError(w, http.StatusConflict, "conversation_taken", err.Error()+"; end that link first")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[chat-links] %s: session %s linked to %s %s conversation %s (%s)", created.ID, created.SessionID, created.Platform, created.AccountID, created.ConversationID, created.PersonDisplayName)
	writeJSON(w, created)
}

// handleListChatLinks: GET /sessions/{id}/chat-links.
func (s *Server) handleListChatLinks(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ListChatLinks(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, links)
}

// handleGetChatLink: GET /sessions/{id}/chat-links/{link_id}, with its log.
func (s *Server) handleGetChatLink(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.GetChatLink(r.PathValue("id"), r.PathValue("link_id"))
	if errors.Is(err, store.ErrChatLinkNotFound) {
		http.Error(w, "chat link not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if link.Messages, err = s.store.ChatLinkMessages(link.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, link)
}

// handleEndChatLink: POST /sessions/{id}/chat-links/{link_id}/end. What the
// person sends after it is not kept; a reply already written is still sent.
func (s *Server) handleEndChatLink(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.EndChatLink(r.PathValue("id"), r.PathValue("link_id"), time.Now().UTC())
	if errors.Is(err, store.ErrChatLinkNotFound) {
		http.Error(w, "chat link not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	log.Printf("[chat-links] %s ended", link.ID)
	writeJSON(w, link)
}

// --- inbound ---

// keepChatLinkInbound keeps one bus message for the active link holding its
// conversation. It answers false with no error for a message no link holds.
func (s *Server) keepChatLinkInbound(inbound chatLinkInbound) (bool, error) {
	if inbound.ReceivingAccountID == "" || inbound.PlatformChannelID == "" {
		return false, nil
	}
	link, found, err := s.store.ActiveChatLinkFor(inbound.ReceivingAccountID, inbound.Platform, inbound.PlatformChannelID)
	if err != nil || !found {
		return false, err
	}
	var attachments []msg.ChatLinkAttachment
	for _, attachment := range inbound.Attachments {
		attachments = append(attachments, msg.ChatLinkAttachment{URL: attachment.URL, Filename: attachment.Filename, ContentType: attachment.ContentType, SizeBytes: attachment.SizeBytes})
	}
	kept, err := s.store.AddInboundChatLinkMessage(store.InboundChatLinkMessage{
		LinkID: link.ID, BusEventID: inbound.EventID, Body: inbound.Body, Attachments: attachments,
		PlatformMessageID: inbound.PlatformMessageID, SenderPlatformUserID: inbound.SenderPlatformUserID,
		SenderPlatformDisplayName: inbound.SenderDisplayName, At: time.Now().UTC()})
	if err != nil {
		return false, err
	}
	if kept {
		log.Printf("[chat-links] %s: kept %s from %s", link.ID, inbound.EventID, link.PersonDisplayName)
	}
	return kept, nil
}

// chatLinkInbound is the part of bus messages.MessagingInbound a chat link
// reads.
type chatLinkInbound struct {
	EventID            string `json:"event_id"`
	Platform           string `json:"platform"`
	Body               string `json:"body"`
	ReceivingAccountID string `json:"receiving_account_id"`
	PlatformChannelID  string `json:"platform_channel_id"`
	PlatformMessageID  string `json:"platform_message_id"`
	// SenderPlatformUserID is the platform's id of who sent it (a Discord
	// user id); SenderDisplayName the name the platform showed.
	SenderPlatformUserID string `json:"sender_platform_user_id"`
	SenderDisplayName    string `json:"sender_display_name"`
	Attachments          []struct {
		URL         string `json:"url"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		SizeBytes   int    `json:"size_bytes"`
	} `json:"attachments"`
}

// --- the sweep ---

// RunChatLinks delivers and sends until ctx ends: on every kick and every
// chatLinkSweepInterval.
func (s *Server) RunChatLinks(ctx context.Context) {
	if s.chatLinks == nil {
		return
	}
	ticker := time.NewTicker(chatLinkSweepInterval)
	defer ticker.Stop()
	for {
		s.sweepChatLinks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.chatLinks.kick:
		}
	}
}

func (s *Server) sweepChatLinks(ctx context.Context) {
	s.chatLinks.mu.Lock()
	defer s.chatLinks.mu.Unlock()
	links, err := s.store.ActiveChatLinks()
	if err != nil {
		log.Printf("[chat-links] ERROR: read active links: %v", err)
		return
	}
	for _, link := range links {
		s.settleChatLinkTurnWithoutEnd(link)
		s.deliverChatLinkInbound(ctx, link)
	}
	s.sendDueChatLinkReplies(ctx)
}

// deliverChatLinkInbound sends a link's pending messages into its session,
// together, when the session is free and no earlier delivery is in it.
func (s *Server) deliverChatLinkInbound(ctx context.Context, link store.ChatLink) {
	inSession, err := s.store.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessageInSession)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: read messages in session: %v", link.ID, err)
		return
	}
	if len(inSession) > 0 {
		return
	}
	pending, err := s.store.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessagePending)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: read pending messages: %v", link.ID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	session, err := s.store.GetSession(link.SessionID)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: session %s: %v", link.ID, link.SessionID, err)
		return
	}
	if !chatLinkSessionIsFree(msg.SessionState(session.State)) {
		return
	}
	pending, err = s.resolveChatLinkSenders(ctx, link, pending)
	if err != nil {
		return
	}
	accountName, err := s.chatLinks.botAccountName(ctx, link.AccountID, link.Platform)
	if err != nil {
		log.Printf("[chat-links] %s: the account's name is unknown (%v); labeling by platform only", link.ID, err)
		accountName = ""
	}
	ids := make([]string, len(pending))
	for index, message := range pending {
		ids[index] = message.ID
	}
	deliveryID := chatLinkDeliveryIDPrefix + pending[0].ID
	status, body := s.chatLinks.sendIntoSession(link.SessionID,
		msg.SendMessageRequest{Message: chatLinkInboundText(link, session.PrincipalID, accountName, pending), ClientRequestID: deliveryID})
	now := s.chatLinks.now()
	if status != http.StatusOK {
		reason := fmt.Sprintf("the session refused it with %d: %s", status, strings.TrimSpace(string(body)))
		log.Printf("[chat-links] ERROR: %s: deliver %v: %s", link.ID, ids, reason)
		if err := s.store.RecordChatLinkDeliveryAttemptFailed(ids, reason, now); err != nil {
			log.Printf("[chat-links] ERROR: %s: record the failed delivery: %v", link.ID, err)
		}
		return
	}
	var sent msg.SendMessageResponse
	if err := json.Unmarshal(body, &sent); err != nil {
		log.Printf("[chat-links] ERROR: %s: the send's answer %q: %v", link.ID, body, err)
	}
	bridgeMessageID := sent.MessageID
	if bridgeMessageID == "" {
		bridgeMessageID = sent.HeldMessageID
	}
	if err := s.store.MarkChatLinkMessagesInSession(ids, deliveryID, bridgeMessageID, now); err != nil {
		log.Printf("[chat-links] ERROR: %s: %v were sent into %s but not marked: %v", link.ID, ids, link.SessionID, err)
		return
	}
	log.Printf("[chat-links] %s: delivered %v into %s as %s", link.ID, ids, link.SessionID, deliveryID)
}

// settleChatLinkTurnWithoutEnd fails a delivery whose session is free again
// with no turn end seen for it: the turn ended in an error or was cut off,
// which reports no turn end.
func (s *Server) settleChatLinkTurnWithoutEnd(link store.ChatLink) {
	inSession, err := s.store.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessageInSession)
	if err != nil || len(inSession) == 0 {
		return
	}
	if s.chatLinks.now().Sub(inSession[0].UpdatedAt) < chatLinkTurnWithoutEndAfter {
		return
	}
	session, err := s.store.GetSession(link.SessionID)
	if err != nil || !chatLinkSessionIsFree(msg.SessionState(session.State)) {
		return
	}
	reason := fmt.Sprintf("the session is %s and no end of the turn was seen, so there is no reply to send", session.State)
	s.finishChatLinkDelivery(link, inSession[0].DeliveryID, "", reason)
}

// onChatLinkTurnEnd takes the final reply of a turn a chat link started.
// Any turn end wakes the sweep, since the session is free now.
func (s *Server) onChatLinkTurnEnd(bridgeID string, ev *msg.Event, state msg.SessionState) {
	if s.chatLinks == nil {
		return
	}
	defer s.chatLinks.wake()
	if ev == nil || !strings.HasPrefix(ev.ClientRequestID, chatLinkDeliveryIDPrefix) {
		return
	}
	delivered, err := s.store.ChatLinkMessagesOfDelivery(ev.ClientRequestID)
	if err != nil || len(delivered) == 0 {
		log.Printf("[chat-links] ERROR: session %s ended turn %s but its messages were not found: %v", bridgeID, ev.ClientRequestID, err)
		return
	}
	link, err := s.store.ChatLinkOfMessage(delivered[0])
	if err != nil {
		log.Printf("[chat-links] ERROR: the link of %s: %v", delivered[0].ID, err)
		return
	}
	reply, reason := "", ""
	switch {
	case ev.Result == nil:
		reason = "the turn ended with no result"
	case ev.Result.IsError || state == msg.SessionError || state == msg.SessionAborted:
		reason = fmt.Sprintf("the turn ended in %s: %s", state, strings.TrimSpace(ev.Result.Text))
	case strings.TrimSpace(ev.Result.Text) == "":
		reason = "the turn's final reply was empty"
	default:
		reply = ev.Result.Text
	}
	s.chatLinks.mu.Lock()
	defer s.chatLinks.mu.Unlock()
	s.finishChatLinkDelivery(link, ev.ClientRequestID, reply, reason)
}

// finishChatLinkDelivery closes a delivery: its inbound messages are
// delivered, and its reply is kept to send, or failed with reason when there
// is none — and then the session is told.
func (s *Server) finishChatLinkDelivery(link store.ChatLink, deliveryID, reply, reason string) {
	now := s.chatLinks.now()
	delivered, err := s.store.ChatLinkMessagesOfDelivery(deliveryID)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: read delivery %s: %v", link.ID, deliveryID, err)
		return
	}
	for _, message := range delivered {
		if message.Direction == msg.ChatLinkInbound && message.State == msg.ChatLinkMessageInSession {
			if err := s.store.SetChatLinkMessageState(message.ID, msg.ChatLinkMessageDelivered, "", nil, now); err != nil {
				log.Printf("[chat-links] ERROR: %s: mark %s delivered: %v", link.ID, message.ID, err)
			}
		}
	}
	state := msg.ChatLinkMessagePending
	if reply == "" {
		state = msg.ChatLinkMessageFailed
	}
	outbound, err := s.store.AddOutboundChatLinkMessage(link.ID, deliveryID, reply, state, reason, now)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: keep the reply to %s: %v", link.ID, deliveryID, err)
		return
	}
	if state == msg.ChatLinkMessageFailed {
		log.Printf("[chat-links] %s: no reply for %s: %s", link.ID, deliveryID, reason)
		s.reportChatLinkFailure(link, outbound, reason)
	}
}

// sendDueChatLinkReplies sends every reply whose attempt is due.
func (s *Server) sendDueChatLinkReplies(ctx context.Context) {
	due, err := s.store.DueOutboundChatLinkMessages(s.chatLinks.now())
	if err != nil {
		log.Printf("[chat-links] ERROR: read due replies: %v", err)
		return
	}
	for _, reply := range due {
		link, err := s.store.ChatLinkOfMessage(reply)
		if err != nil {
			log.Printf("[chat-links] ERROR: the link of %s: %v", reply.ID, err)
			continue
		}
		s.sendChatLinkReply(ctx, link, reply)
	}
}

func (s *Server) sendChatLinkReply(ctx context.Context, link store.ChatLink, reply store.ChatLinkMessage) {
	body := s.chatLinkReplyForRecipient(ctx, link, reply.Body)
	var result multichatSendResult
	status, err := s.chatLinks.call(ctx, http.MethodPost, "/api/messages/send", map[string]string{
		"account_id": link.AccountID, "channel_id": link.ConversationID, "body": body,
	}, &result)
	now := s.chatLinks.now()
	if err == nil {
		if body != reply.Body {
			if err := s.store.SetChatLinkMessageSentBody(reply.ID, body); err != nil {
				log.Printf("[chat-links] ERROR: %s: %s was sent with its references drawn but that was not recorded: %v", link.ID, reply.ID, err)
			}
		}
		if err := s.store.SetChatLinkMessageState(reply.ID, msg.ChatLinkMessageDelivered, "", result.MessageIDs, now); err != nil {
			log.Printf("[chat-links] ERROR: %s: %s was sent as %v but not marked: %v", link.ID, reply.ID, result.MessageIDs, err)
		}
		log.Printf("[chat-links] %s: sent %s to %s as %v", link.ID, reply.ID, link.PersonDisplayName, result.MessageIDs)
		return
	}
	// A refusal that says what was wrong with the send will say it again;
	// a part already sent must not be sent twice.
	partlySent := status == http.StatusBadGateway && strings.Contains(err.Error(), `"message_ids"`)
	if status == http.StatusBadRequest || status == http.StatusNotFound || partlySent || reply.Attempts+1 >= chatLinkSendAttempts {
		reason := err.Error()
		if reply.Attempts+1 >= chatLinkSendAttempts {
			reason = fmt.Sprintf("gave up after %d attempts: %s", reply.Attempts+1, reason)
		}
		if err := s.store.SetChatLinkMessageState(reply.ID, msg.ChatLinkMessageFailed, reason, nil, now); err != nil {
			log.Printf("[chat-links] ERROR: %s: mark %s failed: %v", link.ID, reply.ID, err)
		}
		log.Printf("[chat-links] ERROR: %s: %s will not be sent: %s", link.ID, reply.ID, reason)
		s.reportChatLinkFailure(link, reply, reason)
		return
	}
	wait := chatLinkFirstRetry << reply.Attempts
	if wait <= 0 || wait > chatLinkLongestRetry {
		wait = chatLinkLongestRetry
	}
	if err := s.store.RecordChatLinkSendFailed(reply.ID, err.Error(), now.Add(wait), now); err != nil {
		log.Printf("[chat-links] ERROR: %s: record the failed send of %s: %v", link.ID, reply.ID, err)
	}
	log.Printf("[chat-links] %s: send %s failed (attempt %d), trying again in %s: %v", link.ID, reply.ID, reply.Attempts+1, wait, err)
}

// reportChatLinkFailure tells the session a reply did not reach the person.
// The report's turn carries a report request id, so its reply goes nowhere.
func (s *Server) reportChatLinkFailure(link store.ChatLink, reply store.ChatLinkMessage, reason string) {
	text := fmt.Sprintf("Chat link %s: your reply to %s was not sent (%s). %s has not heard back from you about their last message; reply to this turn goes only to the operator, not to %s.",
		link.ID, link.PersonDisplayName, reason, link.PersonDisplayName, link.PersonDisplayName)
	if reply.Body != "" {
		text += "\n\nThe reply that was not sent:\n\n" + reply.Body
	}
	go func() {
		status, body := s.chatLinks.sendIntoSession(link.SessionID,
			msg.SendMessageRequest{Message: text, ClientRequestID: chatLinkReportRequestIDPrefix + reply.ID})
		if status != http.StatusOK {
			log.Printf("[chat-links] ERROR: %s: session %s was not told that %s failed: %d %s", link.ID, link.SessionID, reply.ID, status, strings.TrimSpace(string(body)))
		}
	}()
}

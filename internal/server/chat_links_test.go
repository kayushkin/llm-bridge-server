package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeMultichat answers GET /api/accounts with one bot and records sends.
type fakeMultichat struct {
	mu         sync.Mutex
	sends      []map[string]string
	sendStatus int
}

func (f *fakeMultichat) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer multichat-token" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/accounts":
		w.Write([]byte(`{"own_accounts":[],"bot_accounts":[{"id":"message_account_000002","platform":"discord","display_name":"Helper Bot","state":"connected"}]}`))
	case "/api/messages/send":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.sends = append(f.sends, body)
		if f.sendStatus != 0 {
			http.Error(w, `{"error":"down"}`, f.sendStatus)
			return
		}
		w.Write([]byte(`{"account_id":"message_account_000002","channel_id":"dm-1","message_ids":["900"]}`))
	default:
		http.NotFound(w, r)
	}
}

type chatLinkTest struct {
	srv             *Server
	st              *store.Store
	multichat       *fakeMultichat
	multichatServer *httptest.Server
	sentMu          sync.Mutex
	sentLog         []msg.SendMessageRequest
	clock           time.Time
}

// sent answers what went into the session so far; a report goes in from
// a goroutine of its own.
func (c *chatLinkTest) sentSoFar() []msg.SendMessageRequest {
	c.sentMu.Lock()
	defer c.sentMu.Unlock()
	return append([]msg.SendMessageRequest(nil), c.sentLog...)
}

func (c *chatLinkTest) newChatLinks(multichat *httptest.Server) *chatLinks {
	return &chatLinks{
		multichatURL: multichat.URL, httpClient: multichat.Client(),
		token: func(context.Context) (string, error) { return "multichat-token", nil },
		kick:  make(chan struct{}, 1),
		now:   func() time.Time { return c.clock },
		sendIntoSession: func(sessionID string, request msg.SendMessageRequest) (int, []byte) {
			c.sentMu.Lock()
			defer c.sentMu.Unlock()
			c.sentLog = append(c.sentLog, request)
			return http.StatusOK, []byte(`{"status":"sent","message_id":"msg_x"}`)
		},
	}
}

func newChatLinkTest(t *testing.T) *chatLinkTest {
	t.Helper()
	srv, st := testServer(t)
	test := &chatLinkTest{srv: srv, st: st, multichat: &fakeMultichat{}, clock: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)}
	multichat := httptest.NewServer(http.HandlerFunc(test.multichat.handler))
	t.Cleanup(multichat.Close)
	test.multichatServer = multichat
	srv.chatLinks = test.newChatLinks(multichat)
	newSessionForSignals(t, st, "br_linked", msg.SessionTypeInteractive)
	return test
}

func (c *chatLinkTest) createLink(t *testing.T) store.ChatLink {
	t.Helper()
	recorder := httptest.NewRecorder()
	asInternalService(c.srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/br_linked/chat-links", strings.NewReader(
		`{"account_id":"message_account_000002","platform":"discord","conversation_id":"dm-1","person_display_name":"Carol"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body)
	}
	var link store.ChatLink
	json.Unmarshal(recorder.Body.Bytes(), &link)
	return link
}

func (c *chatLinkTest) setState(t *testing.T, state msg.SessionState) {
	t.Helper()
	if _, err := c.st.WriteSessionStatus("br_linked", &msg.Event{Type: msg.EventSessionStatus, Status: &msg.SessionStatus{State: state, Reason: msg.ReasonLifecycle}}); err != nil {
		t.Fatal(err)
	}
}

func (c *chatLinkTest) receive(t *testing.T, eventID, body string) {
	t.Helper()
	if _, err := c.srv.keepChatLinkInbound(chatLinkInbound{EventID: eventID, Platform: "discord", Body: body,
		ReceivingAccountID: "message_account_000002", PlatformChannelID: "dm-1", PlatformMessageID: strings.TrimPrefix(eventID, "discord:")}); err != nil {
		t.Fatal(err)
	}
}

func TestChatLinkInboundTextLabelsThePerson(t *testing.T) {
	link := store.ChatLink{ID: "chat_link_000001", Platform: "discord", PersonDisplayName: "Carol"}
	text := chatLinkInboundText(link, "Helper Bot", []store.ChatLinkMessage{
		{Body: "can the timer be 30 seconds?"},
		{Body: "", Attachments: []msg.ChatLinkAttachment{{URL: "https://cdn.example/sketch.png", Filename: "sketch.png", ContentType: "image/png", SizeBytes: 12}}},
	})
	for _, want := range []string{"Carol wrote to you on Discord, through the Helper Bot bot:", "can the timer be 30 seconds?",
		"[attached file: sketch.png (image/png, 12 bytes) https://cdn.example/sketch.png]", "This is from Carol, not the operator.",
		"Your final reply in this turn is sent to Carol word for word", "chat_link_000001"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestChatLinksOffRefuseALink(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_off", msg.SessionTypeInteractive)
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/br_off/chat-links", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "LLMBRIDGE_NATS_URL") {
		t.Fatalf("off: %d %s", recorder.Code, recorder.Body)
	}
}

func TestAChatLinkNeedsABotMultichatHolds(t *testing.T) {
	c := newChatLinkTest(t)
	for name, body := range map[string]string{
		"unknown account": `{"account_id":"message_account_000099","platform":"discord","conversation_id":"dm-1","person_display_name":"Carol"}`,
		"wrong platform":  `{"account_id":"message_account_000002","platform":"slack","conversation_id":"dm-1","person_display_name":"Carol"}`,
		"no conversation": `{"account_id":"message_account_000002","platform":"discord","person_display_name":"Carol"}`,
		"unknown field":   `{"account":"message_account_000002"}`,
	} {
		recorder := httptest.NewRecorder()
		asInternalService(c.srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/br_linked/chat-links", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, recorder.Code, recorder.Body)
		}
	}
	c.createLink(t)
	recorder := httptest.NewRecorder()
	asInternalService(c.srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/br_linked/chat-links", strings.NewReader(
		`{"account_id":"message_account_000002","platform":"discord","conversation_id":"dm-1","person_display_name":"Carol"}`)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("second link to one conversation: %d", recorder.Code)
	}
}

func TestChatLinkRoundTripSendsOnlyTheTurnItStarted(t *testing.T) {
	c := newChatLinkTest(t)
	link := c.createLink(t)
	ctx := context.Background()

	// The session is busy: messages wait, and two that arrive are sent together.
	c.setState(t, msg.SessionToolRunning)
	c.receive(t, "discord:10", "hi")
	c.receive(t, "discord:10", "hi") // JetStream redelivers
	c.receive(t, "discord:11", "are you there?")
	c.srv.sweepChatLinks(ctx)
	if len(c.sentSoFar()) != 0 {
		t.Fatalf("sent into a busy session: %+v", c.sentSoFar())
	}

	// A turn the operator started ends: nothing goes to the person.
	c.setState(t, msg.SessionIdle)
	c.srv.onChatLinkTurnEnd("br_linked", &msg.Event{Type: msg.EventResult, ClientRequestID: "", Result: &msg.ResultEvent{Text: "for the operator"}}, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 0 {
		t.Fatalf("an operator's turn reached the person: %+v", c.multichat.sends)
	}
	if len(c.sentSoFar()) != 1 || !strings.Contains(c.sentSoFar()[0].Message, "hi\n\n---\n\nare you there?") || !strings.HasPrefix(c.sentSoFar()[0].ClientRequestID, chatLinkDeliveryIDPrefix) {
		t.Fatalf("delivery: %+v", c.sentSoFar())
	}
	deliveryID := c.sentSoFar()[0].ClientRequestID

	// The person writes again while that turn runs: it waits for the turn.
	c.setState(t, msg.SessionModelGenerating)
	c.receive(t, "discord:12", "also, more strikes?")
	c.srv.sweepChatLinks(ctx)
	if len(c.sentSoFar()) != 1 {
		t.Fatalf("a second delivery went in while the first was in the session: %d", len(c.sentSoFar()))
	}

	// The link's turn ends: its final reply goes to the person, as the bot, to the conversation.
	c.setState(t, msg.SessionIdle)
	c.srv.onChatLinkTurnEnd("br_linked", &msg.Event{Type: msg.EventResult, ClientRequestID: deliveryID, Result: &msg.ResultEvent{Text: "Hi Carol!"}}, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 1 || c.multichat.sends[0]["body"] != "Hi Carol!" || c.multichat.sends[0]["account_id"] != "message_account_000002" || c.multichat.sends[0]["channel_id"] != "dm-1" {
		t.Fatalf("reply: %+v", c.multichat.sends)
	}
	// ...and the waiting message is delivered next.
	if len(c.sentSoFar()) != 2 || !strings.Contains(c.sentSoFar()[1].Message, "also, more strikes?") {
		t.Fatalf("the waiting message: %+v", c.sentSoFar())
	}

	recorder := httptest.NewRecorder()
	asInternalService(c.srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/br_linked/chat-links/"+link.ID, nil))
	var read store.ChatLink
	json.Unmarshal(recorder.Body.Bytes(), &read)
	states := []string{}
	for _, message := range read.Messages {
		states = append(states, string(message.Direction)+":"+string(message.State))
	}
	if strings.Join(states, ",") != "inbound:delivered,inbound:delivered,inbound:in_session,outbound:delivered" {
		t.Fatalf("log: %v", states)
	}
	if read.Messages[3].PlatformMessageIDs[0] != "900" {
		t.Fatalf("reply ids: %+v", read.Messages[3])
	}
}

func TestAChatLinkReplyIsRetriedAndAFailedTurnIsReported(t *testing.T) {
	c := newChatLinkTest(t)
	c.createLink(t)
	ctx := context.Background()
	c.receive(t, "discord:20", "hello")
	c.srv.sweepChatLinks(ctx)
	deliveryID := c.sentSoFar()[0].ClientRequestID

	c.multichat.sendStatus = http.StatusServiceUnavailable
	c.srv.onChatLinkTurnEnd("br_linked", &msg.Event{Type: msg.EventResult, ClientRequestID: deliveryID, Result: &msg.ResultEvent{Text: "reply"}}, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 1 {
		t.Fatalf("first try: %d", len(c.multichat.sends))
	}
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 1 {
		t.Fatal("retried before its wait")
	}
	c.multichat.sendStatus = 0
	c.clock = c.clock.Add(time.Minute)
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 2 {
		t.Fatalf("retry after the wait: %d", len(c.multichat.sends))
	}

	// A turn that ends in an error sends nothing and tells the session, in a
	// turn whose reply goes nowhere.
	c.receive(t, "discord:21", "and?")
	c.srv.sweepChatLinks(ctx)
	second := c.sentSoFar()[len(c.sentSoFar())-1].ClientRequestID
	c.srv.onChatLinkTurnEnd("br_linked", &msg.Event{Type: msg.EventResult, ClientRequestID: second, Result: &msg.ResultEvent{Text: "boom", IsError: true}}, msg.SessionError)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.HasPrefix(c.sentSoFar()[len(c.sentSoFar())-1].ClientRequestID, chatLinkReportRequestIDPrefix) {
		time.Sleep(10 * time.Millisecond)
	}
	report := c.sentSoFar()[len(c.sentSoFar())-1]
	if !strings.HasPrefix(report.ClientRequestID, chatLinkReportRequestIDPrefix) || !strings.Contains(report.Message, "was not sent") {
		t.Fatalf("report: %+v", report)
	}
	c.srv.onChatLinkTurnEnd("br_linked", &msg.Event{Type: msg.EventResult, ClientRequestID: report.ClientRequestID, Result: &msg.ResultEvent{Text: "noted"}}, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if len(c.multichat.sends) != 2 {
		t.Fatalf("a report's turn reached the person: %+v", c.multichat.sends)
	}
}

func TestAChatLinkTurnThatNeverEndsIsSettledAndPendingMessagesSurviveARestart(t *testing.T) {
	c := newChatLinkTest(t)
	c.createLink(t)
	ctx := context.Background()
	c.setState(t, msg.SessionModelGenerating)
	c.receive(t, "discord:30", "kept across a restart")

	// A new process: the same store, a fresh chatLinks.
	c.srv.chatLinks = c.newChatLinks(c.multichatServer)
	c.setState(t, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if len(c.sentSoFar()) != 1 || !strings.Contains(c.sentSoFar()[0].Message, "kept across a restart") {
		t.Fatalf("pending message after restart: %+v", c.sentSoFar())
	}

	// No turn end arrives (an error-terminated turn reports none); once the
	// session has been free long enough the delivery is failed and reported.
	c.clock = c.clock.Add(chatLinkTurnWithoutEndAfter + time.Second)
	c.srv.sweepChatLinks(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.sentSoFar()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.sentSoFar()) != 2 || !strings.HasPrefix(c.sentSoFar()[1].ClientRequestID, chatLinkReportRequestIDPrefix) {
		t.Fatalf("no report of the turn without an end: %+v", c.sentSoFar())
	}
}

func TestASessionsAgentChatLinksItsOwnSessionAndNoOther(t *testing.T) {
	c := newChatLinkTest(t)
	c.srv.cfg.ListenAddr = "127.0.0.1:8160" // the posting token is given only with an address to post to
	session, _ := c.st.GetSession("br_linked")
	newSessionForSignals(t, c.st, "br_other", msg.SessionTypeInteractive)
	environment := map[string]string{}
	for _, variable := range c.srv.sessionPostingEnvironment(session) {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	token := environment[sessionPostingTokenEnvironmentVariable]
	body := `{"account_id":"message_account_000002","platform":"discord","conversation_id":"dm-1","person_display_name":"Carol"}`
	withToken := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		c.srv.ServeHTTP(recorder, request)
		return recorder
	}
	if code := withToken(http.MethodPost, "/sessions/br_other/chat-links", body).Code; code == http.StatusOK {
		t.Fatal("an agent linked another session")
	}
	recorder := withToken(http.MethodPost, "/sessions/br_linked/chat-links", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent links its own session: %d %s", recorder.Code, recorder.Body)
	}
	var link store.ChatLink
	json.Unmarshal(recorder.Body.Bytes(), &link)
	if !link.CreatedBySessionAgent {
		t.Errorf("link not marked as the agent's: %+v", link)
	}
	if code := withToken(http.MethodGet, "/sessions/br_linked/chat-links/"+link.ID, "").Code; code != http.StatusOK {
		t.Errorf("agent reads its link: %d", code)
	}
	if code := withToken(http.MethodPost, "/sessions/br_linked/chat-links/"+link.ID+"/end", "").Code; code != http.StatusOK {
		t.Errorf("agent ends its link: %d", code)
	}
}

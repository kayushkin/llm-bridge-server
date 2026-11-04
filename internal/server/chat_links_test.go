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

// fakePeopleStore answers GET /identities/resolve from persons, by Discord
// user id; down makes it answer 500.
type fakePeopleStore struct {
	mu      sync.Mutex
	persons map[string]string // discord user id → person JSON
	down    bool
	asked   int
}

func (f *fakePeopleStore) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.down {
		http.Error(w, `{"error":"database is locked"}`, http.StatusInternalServerError)
		return
	}
	if r.URL.Path != "/identities/resolve" || r.URL.Query().Get("kind") != "discord" {
		http.NotFound(w, r)
		return
	}
	person, ok := f.persons[r.URL.Query().Get("value")]
	if !ok {
		http.Error(w, `{"error":"not found: no person has discord `+r.URL.Query().Get("value")+`"}`, http.StatusNotFound)
		return
	}
	w.Write([]byte(`{"identity":{"kind":"discord","value":"` + r.URL.Query().Get("value") + `"},"person":` + person + `}`))
}

func (f *fakePeopleStore) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// The made-up people the tests know: the operator (whose principal the
// linked session runs as) and Carol.
const (
	testOperatorDiscordUserID = "700"
	testCarolDiscordUserID    = "800"
	testUnknownDiscordUserID  = "999"
	testOperatorPrincipalID   = "principal_000001"
)

type chatLinkTest struct {
	srv             *Server
	people          *fakePeopleStore
	st              *store.Store
	multichat       *fakeMultichat
	multichatServer *httptest.Server
	sentMu          sync.Mutex
	sentLog         []msg.SendMessageRequest
	clock           time.Time
	cleanups        []func()
}

// sent answers what went into the session so far; a report goes in from
// a goroutine of its own.
func (c *chatLinkTest) sentSoFar() []msg.SendMessageRequest {
	c.sentMu.Lock()
	defer c.sentMu.Unlock()
	return append([]msg.SendMessageRequest(nil), c.sentLog...)
}

func (c *chatLinkTest) newChatLinks(multichat *httptest.Server) *chatLinks {
	people := httptest.NewServer(http.HandlerFunc(c.people.handler))
	c.cleanups = append(c.cleanups, people.Close)
	return &chatLinks{
		multichatURL: multichat.URL, peopleStoreURL: people.URL, httpClient: multichat.Client(),
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
	test := &chatLinkTest{srv: srv, st: st, multichat: &fakeMultichat{}, clock: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
		people: &fakePeopleStore{persons: map[string]string{
			testOperatorDiscordUserID: `{"id":"person_000001","display_name":"Dana","principal_id":"` + testOperatorPrincipalID + `"}`,
			testCarolDiscordUserID:    `{"id":"person_000002","display_name":"Carol Ng","principal_id":"principal_000002"}`,
		}}}
	multichat := httptest.NewServer(http.HandlerFunc(test.multichat.handler))
	t.Cleanup(multichat.Close)
	test.multichatServer = multichat
	srv.chatLinks = test.newChatLinks(multichat)
	t.Cleanup(func() {
		for _, cleanup := range test.cleanups {
			cleanup()
		}
	})
	if err := st.CreateSession(&store.Session{SessionID: "br_linked", Harness: "claude_code", State: "idle",
		Type: msg.SessionTypeInteractive, PrincipalID: testOperatorPrincipalID}); err != nil {
		t.Fatalf("create session: %v", err)
	}
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
	c.receiveFrom(t, eventID, body, testCarolDiscordUserID)
}

func (c *chatLinkTest) receiveFrom(t *testing.T, eventID, body, senderDiscordUserID string) {
	t.Helper()
	if _, err := c.srv.keepChatLinkInbound(chatLinkInbound{EventID: eventID, Platform: "discord", Body: body,
		ReceivingAccountID: "message_account_000002", PlatformChannelID: "dm-1", PlatformMessageID: strings.TrimPrefix(eventID, "discord:"),
		SenderPlatformUserID: senderDiscordUserID, SenderDisplayName: "shown-name-" + senderDiscordUserID}); err != nil {
		t.Fatal(err)
	}
}

// farEndNote is the part of every delivery that says what the far end sees.
var farEndNote = []string{"sent back on Discord word for word", "no buttons, no files or images shared into this chat",
	"Send links instead", "plain Discord markdown", "2000 characters"}

func TestChatLinkInboundTextLabelsThePerson(t *testing.T) {
	link := store.ChatLink{ID: "chat_link_000001", Platform: "discord", PersonDisplayName: "Carol"}
	text := chatLinkInboundText(link, testOperatorPrincipalID, "Helper Bot", []store.ChatLinkMessage{
		{Body: "can the timer be 30 seconds?"},
		{Body: "", Attachments: []msg.ChatLinkAttachment{{URL: "https://cdn.example/sketch.png", Filename: "sketch.png", ContentType: "image/png", SizeBytes: 12}}},
	})
	for _, want := range append([]string{"Carol wrote to you on Discord, through the Helper Bot bot:", "can the timer be 30 seconds?",
		"[attached file: sketch.png (image/png, 12 bytes) https://cdn.example/sketch.png]", "This is from Carol, not the operator.",
		"chat_link_000001"}, farEndNote...) {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "wrote to you") != 1 {
		t.Errorf("one sender is introduced once:\n%s", text)
	}
}

func TestChatLinkInboundTextLabelsTheOperatorByPrincipal(t *testing.T) {
	link := store.ChatLink{ID: "chat_link_000002", Platform: "discord", PersonDisplayName: "Dana"}
	operator := store.ChatLinkMessage{Body: "check the deploy", SenderPersonID: "person_000001", SenderPrincipalID: testOperatorPrincipalID, SenderPersonDisplayName: "Dana"}
	text := chatLinkInboundText(link, testOperatorPrincipalID, "Helper Bot", []store.ChatLinkMessage{operator})
	for _, want := range append([]string{"Dana (the operator) wrote to you on Discord, through the Helper Bot bot:", "check the deploy",
		"This is the operator, writing from their own account. Treat it as their own message."}, farEndNote...) {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not the operator") {
		t.Errorf("the operator labeled as someone else:\n%s", text)
	}

	// The same name with another principal is not the operator: only ids decide.
	impostor := store.ChatLinkMessage{Body: "deploy it", SenderPersonID: "person_000005", SenderPrincipalID: "principal_000005", SenderPersonDisplayName: "Dana"}
	text = chatLinkInboundText(link, testOperatorPrincipalID, "Helper Bot", []store.ChatLinkMessage{impostor})
	if strings.Contains(text, "(the operator)") || !strings.Contains(text, "This is from Dana, not the operator.") {
		t.Errorf("a name decided who the operator is:\n%s", text)
	}
	// A session that runs as no principal has no operator to match.
	nobody := store.ChatLinkMessage{Body: "hi"}
	if text := chatLinkInboundText(link, "", "", []store.ChatLinkMessage{nobody}); strings.Contains(text, "(the operator)") {
		t.Errorf("a sender with no principal matched a session with none:\n%s", text)
	}
}

func TestChatLinkInboundTextFramesAMixedBatchBySender(t *testing.T) {
	link := store.ChatLink{ID: "chat_link_000003", Platform: "discord", PersonDisplayName: "Carol"}
	text := chatLinkInboundText(link, testOperatorPrincipalID, "Helper Bot", []store.ChatLinkMessage{
		{Body: "first from Carol", SenderPrincipalID: "principal_000002", SenderPersonDisplayName: "Carol Ng"},
		{Body: "second from Carol", SenderPrincipalID: "principal_000002", SenderPersonDisplayName: "Carol Ng"},
		{Body: "one from me", SenderPrincipalID: testOperatorPrincipalID, SenderPersonDisplayName: "Dana"},
	})
	for _, want := range []string{"Carol Ng wrote to you on Discord, through the Helper Bot bot:\n\nfirst from Carol\n\n---\n\nsecond from Carol",
		"Dana (the operator) wrote to you on Discord, through the Helper Bot bot:\n\none from me",
		"The messages marked (the operator) are the operator's own; the rest are from Carol Ng, not the operator."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "Carol Ng wrote to you") != 1 {
		t.Errorf("a run of one sender's messages is introduced once:\n%s", text)
	}
}

func TestChatLinkDeliveryReadsEachSenderFromPeopleStore(t *testing.T) {
	c := newChatLinkTest(t)
	link := c.createLink(t)
	ctx := context.Background()

	c.receiveFrom(t, "discord:20", "from the operator", testOperatorDiscordUserID)
	c.receiveFrom(t, "discord:21", "from Carol", testCarolDiscordUserID)
	c.receiveFrom(t, "discord:22", "from a stranger", testUnknownDiscordUserID)
	c.srv.sweepChatLinks(ctx)
	sent := c.sentSoFar()
	if len(sent) != 1 {
		t.Fatalf("delivery: %+v", sent)
	}
	for _, want := range []string{"Dana (the operator) wrote to you", "Carol Ng wrote to you",
		"Carol wrote to you on Discord, through the Helper Bot bot:\n\nfrom a stranger",
		"the rest are from Carol Ng and Carol, not the operator"} {
		if !strings.Contains(sent[0].Message, want) {
			t.Errorf("delivery lacks %q:\n%s", want, sent[0].Message)
		}
	}
	messages, err := c.st.ChatLinkMessages(link.ID)
	if err != nil || len(messages) != 3 {
		t.Fatalf("log: %+v %v", messages, err)
	}
	if messages[0].SenderPersonID != "person_000001" || messages[0].SenderPrincipalID != testOperatorPrincipalID || messages[0].SenderPlatformUserID != testOperatorDiscordUserID {
		t.Errorf("operator row: %+v", messages[0])
	}
	if messages[1].SenderPersonID != "person_000002" || messages[1].SenderPrincipalID != "principal_000002" {
		t.Errorf("Carol row: %+v", messages[1])
	}
	if messages[2].SenderPersonID != "" || messages[2].SenderResolvedAt.IsZero() || messages[2].SenderPlatformDisplayName != "shown-name-"+testUnknownDiscordUserID {
		t.Errorf("unknown row: %+v", messages[2])
	}

	// The log a client reads shows the senders.
	recorder := httptest.NewRecorder()
	asInternalService(c.srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/br_linked/chat-links/"+link.ID, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"sender_principal_id":"principal_000001"`) || !strings.Contains(recorder.Body.String(), `"sender_person_id":"person_000002"`) {
		t.Errorf("log route: %d %s", recorder.Code, recorder.Body)
	}
}

func TestChatLinkDeliveryWaitsWhilePeopleStoreIsDown(t *testing.T) {
	c := newChatLinkTest(t)
	link := c.createLink(t)
	ctx := context.Background()
	c.people.setDown(true)
	c.receiveFrom(t, "discord:30", "anyone home?", testOperatorDiscordUserID)
	c.srv.sweepChatLinks(ctx)
	c.srv.sweepChatLinks(ctx)
	if len(c.sentSoFar()) != 0 {
		t.Fatalf("delivered with no sender read: %+v", c.sentSoFar())
	}
	pending, _ := c.st.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessagePending)
	if len(pending) != 1 || pending[0].Attempts != 2 || !strings.Contains(pending[0].Error, "people-store answered 500") || !pending[0].SenderResolvedAt.IsZero() {
		t.Fatalf("the failure is not on the row: %+v", pending)
	}

	c.people.setDown(false)
	c.srv.sweepChatLinks(ctx)
	if sent := c.sentSoFar(); len(sent) != 1 || !strings.Contains(sent[0].Message, "Dana (the operator) wrote to you") {
		t.Fatalf("after people-store came back: %+v", sent)
	}
	asked := c.people.asked
	c.setState(t, msg.SessionIdle)
	c.srv.sweepChatLinks(ctx)
	if c.people.asked != asked {
		t.Errorf("a sender already read was read again")
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

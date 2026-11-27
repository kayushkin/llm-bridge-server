package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// mailstackTestServer is a kanbanTestServer with mailstack in the same
// process, as main wires it: one local account, desk, holding two messages,
// the first linked by a card on the board the member may view.
type mailstackTestServer struct {
	*kanbanTestServer
	linkedMessageID, unlinkedMessageID string
}

func newMailstackTestServer(t *testing.T) *mailstackTestServer {
	t.Helper()
	test := &mailstackTestServer{kanbanTestServer: newKanbanTestServer(t)}
	module, err := mailmodule.Module()
	if err != nil {
		t.Fatal(err)
	}
	fileModule, err := files.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module, fileModule)
	db := mailmodule.New(database.Open(module))
	files, err := files.New(database.Open(fileModule), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := test.server.EnableFiles(files); err != nil {
		t.Fatal(err)
	}
	mailAccounts := accounts.New(db)
	if err := mailAccounts.Save(&accounts.Account{ID: "desk", Label: "Desk", Provider: accounts.ProviderLocal, Address: "desk@demo.invalid", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	local := localmail.New(db)
	test.server.EnableMail(mail.NewServer(mail.ServerParts{
		Accounts: mailAccounts, LocalMail: local, People: test.server.MailstackPeopleDirectory(test.stores),
		Files: test.server.MailstackFileStore(),
	}))
	outcomes, _, _, err := local.Import(context.Background(), "desk", []localmail.ImportedMessage{
		importedTestMessage("SRC-1", "Printer jammed again"),
		importedTestMessage("SRC-2", "Salary review"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	test.linkedMessageID, test.unlinkedMessageID = outcomes[0].ID, outcomes[1].ID
	if _, err := test.kanban.CreateCardLink(test.seenCardID, &model.CreateCardLinkRequest{EntityType: "email", EntityRef: "desk:" + test.linkedMessageID}); err != nil {
		t.Fatalf("link the first message to the member's card: %v", err)
	}
	// The second message hangs on a card the member cannot see.
	if _, err := test.kanban.CreateCardLink(test.unseenCardID, &model.CreateCardLinkRequest{EntityType: "email", EntityRef: "desk:" + test.unlinkedMessageID}); err != nil {
		t.Fatalf("link the second message to the other card: %v", err)
	}
	return test
}

func importedTestMessage(providerID, subject string) localmail.ImportedMessage {
	body := "please look at this"
	return localmail.ImportedMessage{ProviderMessageID: providerID, Subject: subject,
		From: backend.Address{Name: "Pat", Email: "pat@demo.invalid"}, ReceivedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC), BodyText: &body}
}

func (test *mailstackTestServer) metadataPath(id string) string {
	return "/mail/api/messages/" + id + "/metadata?account=desk"
}

func TestMailstackAnswersTheInternalServiceAndAnAdministratorWholly(t *testing.T) {
	test := newMailstackTestServer(t)
	for _, caller := range []accessTestCaller{callerServiceTokenAlone, callerAdministratorThroughDash, callerAdministratorWithLoginCookie} {
		for _, path := range []string{"/mail/api/accounts", "/mail/api/messages?account=desk",
			test.metadataPath(test.linkedMessageID), test.metadataPath(test.unlinkedMessageID), "/mail/api/ingress/receipts"} {
			if response := test.call(t, caller, "GET", path, ""); response.Code != http.StatusOK {
				t.Errorf("%s: GET %s = %d %s", caller.name, path, response.Code, response.Body)
			}
		}
	}
}

func TestMailstackNeedsACaller(t *testing.T) {
	test := newMailstackTestServer(t)
	if response := test.call(t, callerWithNoCredential, "GET", "/mail/api/accounts", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d %s, want 401", response.Code, response.Body)
	}
}

// A member holds no mail grant, so it reads only what its tickets show, and
// whatever store token it forges it reads as itself. Naming the
// administrator in X-Principal-Id beside its own cookie gets it nowhere.
func TestMailstackNarrowsAMemberToItsTicketsAndItsGrants(t *testing.T) {
	test := newMailstackTestServer(t)
	forging := accessTestCaller{"member with a login cookie naming the administrator", func(test *accessTestServer, request *http.Request) {
		addLoginCookie(request, test.memberCookie)
		request.Header.Set(principalIdentityHeader, accessTestAdministratorID)
	}}
	for _, path := range []string{test.metadataPath(test.unlinkedMessageID), "/mail/api/accounts"} {
		if response := test.call(t, forging, "GET", path, ""); response.Code != http.StatusUnauthorized {
			t.Errorf("%s: GET %s = %d %s, want 401", forging.name, path, response.Code, response.Body)
		}
	}
	for _, caller := range []accessTestCaller{callerMemberThroughDash, callerMemberWithLoginCookie} {
		for path, want := range map[string]int{
			test.metadataPath(test.linkedMessageID):   http.StatusOK,
			test.metadataPath(test.unlinkedMessageID): http.StatusNotFound,
			"/mail/api/messages?account=desk":         http.StatusForbidden,
			"/mail/api/ingress/receipts":              http.StatusForbidden,
		} {
			if response := test.call(t, caller, "GET", path, ""); response.Code != want {
				t.Errorf("%s: GET %s = %d %s, want %d", caller.name, path, response.Code, response.Body, want)
			}
		}
		var listed struct {
			Accounts []struct {
				ID string `json:"id"`
			} `json:"accounts"`
		}
		response := test.call(t, caller, "GET", "/mail/api/accounts", "")
		if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || response.Code != http.StatusOK || len(listed.Accounts) != 0 {
			t.Errorf("%s: the account list of a member with no grant = %d %s", caller.name, response.Code, response.Body)
		}
	}

	// A can_read_mail grant on desk, checked against mailstack in this
	// process, opens the account.
	ctx := context.Background()
	checker := test.server.grantResourceChecker(test.stores.Principals)
	if _, _, err := test.stores.Grants.Create(ctx, test.grantsDirectory(), checker, grants.GrantRequest{
		PrincipalID: accessTestMemberID, Relation: grants.RelationCanReadMail, ResourceType: grants.ResourceTypeMailAccount, ResourceID: "nowhere",
	}); !errors.Is(err, grants.ErrInvalidGrant) || !strings.Contains(err.Error(), "does not exist in mailstack") {
		t.Fatalf("a grant on an account mailstack does not have: %v, want it refused as one mailstack does not have", err)
	}
	if _, _, err := test.stores.Grants.Create(ctx, test.grantsDirectory(), checker, grants.GrantRequest{
		PrincipalID: accessTestMemberID, Relation: grants.RelationCanReadMail, ResourceType: grants.ResourceTypeMailAccount, ResourceID: "desk",
	}); err != nil {
		t.Fatalf("grant can_read_mail on desk: %v", err)
	}
	for _, path := range []string{test.metadataPath(test.unlinkedMessageID), "/mail/api/messages?account=desk"} {
		if response := test.call(t, callerMemberWithLoginCookie, "GET", path, ""); response.Code != http.StatusOK {
			t.Errorf("with can_read_mail: GET %s = %d %s", path, response.Code, response.Body)
		}
	}
	// Reading is not importing.
	if response := test.call(t, callerMemberWithLoginCookie, "GET", "/mail/api/accounts/desk/import-sources", ""); response.Code != http.StatusForbidden {
		t.Errorf("import sources with can_read_mail only = %d %s, want 403", response.Code, response.Body)
	}
}

// Pushed delivery takes no login: the request reaches mailstack, which
// answers for the ingress credential.
func TestMailstackTakesAPushedDeliveryWithAnIngressCredentialAndNoLogin(t *testing.T) {
	test := newMailstackTestServer(t)
	path := "/mail/api/ingress/deliveries"
	body := `{"deliveries":[{"delivery_id":"d1","account_id":"desk","message":{"provider_message_id":"SRC-9","source_revision":"r1",
		"subject":"Pushed","from":{"email":"pat@demo.invalid"},"received_at":"2026-10-02T09:30:00Z","body_text":"hello"}}]}`
	response := test.call(t, callerWithNoCredential, "POST", path, body)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), mail.IngressTokenHeader) {
		t.Fatalf("no ingress token: %d %s, want mailstack's 401 naming its header", response.Code, response.Body)
	}

	minted := test.call(t, callerServiceTokenAlone, "POST", "/mail/api/ingress/credentials", `{"source":"custody","account_ids":["desk"]}`)
	var credential struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &credential); err != nil || credential.Token == "" {
		t.Fatalf("mint a credential: %d %s", minted.Code, minted.Body)
	}
	delivering := accessTestCaller{"a sender with an ingress credential", func(_ *accessTestServer, request *http.Request) {
		request.Header.Set(mail.IngressTokenHeader, credential.Token)
	}}
	response = test.call(t, delivering, "POST", path, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"accepted"`) {
		t.Fatalf("delivery: %d %s", response.Code, response.Body)
	}
	// The credential opens nothing else.
	if response := test.call(t, delivering, "GET", "/mail/api/accounts", ""); response.Code != http.StatusUnauthorized {
		t.Errorf("the ingress credential on another route = %d %s, want 401", response.Code, response.Body)
	}
}

// Triage's sender lookup reads mailstack in this process.
func TestTriageReadsAMessagesSenderFromMailstackInProcess(t *testing.T) {
	test := newMailstackTestServer(t)
	headers, err := readMailMessageHeaders(context.Background(), test.server.mailMessages, "desk", test.linkedMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if headers.From.Email != "pat@demo.invalid" || headers.Subject != "Printer jammed again" {
		t.Errorf("headers %+v", headers)
	}
}

// An imported attachment's bytes go to the file-store module in this
// process, and come back through mailstack, which checks the message first.
func TestMailstackKeepsAttachmentBytesInTheFileStoreModule(t *testing.T) {
	test := newMailstackTestServer(t)
	body := `{"messages":[{"provider_message_id":"SRC-ATT","subject":"Invoice","from":{"email":"pat@demo.invalid"},
		"received_at":"2026-10-03T09:30:00Z","body_text":"attached",
		"attachments":[{"filename":"invoice.txt","content_type":"text/plain","content_base64":"aW52b2ljZSBib2R5"}]}]}`
	imported := test.call(t, callerServiceTokenAlone, "POST", "/mail/api/accounts/desk/messages/import", body)
	var answer struct {
		Outcomes []struct {
			ID string `json:"id"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &answer); err != nil || imported.Code != http.StatusOK || len(answer.Outcomes) != 1 {
		t.Fatalf("import: %d %s", imported.Code, imported.Body)
	}
	path := "/mail/api/messages/" + answer.Outcomes[0].ID + "/attachments/a1?account=desk"
	read := test.call(t, callerServiceTokenAlone, "GET", path, "")
	if read.Code != http.StatusOK || read.Body.String() != "invoice body" {
		t.Fatalf("read the attachment: %d %q", read.Code, read.Body.String())
	}
	// The member holds no grant and no ticket links this message.
	if response := test.call(t, callerMemberWithLoginCookie, "GET", path, ""); response.Code != http.StatusNotFound {
		t.Errorf("the member reading it: %d %s, want 404", response.Code, response.Body)
	}
}

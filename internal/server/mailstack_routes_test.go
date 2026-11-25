package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/mailmodule"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/model"
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
	db := mailmodule.New(postgrestest.NewDatabase(t, module).Open(module))
	mailAccounts := accounts.New(db)
	if err := mailAccounts.Save(&accounts.Account{ID: "desk", Label: "Desk", Provider: accounts.ProviderLocal, Address: "desk@demo.invalid", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	local := localmail.New(db)
	test.server.EnableMailstack(mailstack.NewServer(mailstack.ServerParts{
		Accounts: mailAccounts, LocalMail: local, People: test.server.MailstackPeopleDirectory(test.stores),
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
	return "/mailstack/api/messages/" + id + "/metadata?account=desk"
}

func TestMailstackAnswersTheInternalServiceAndAnAdministratorWholly(t *testing.T) {
	test := newMailstackTestServer(t)
	for _, caller := range []accessTestCaller{callerServiceTokenAlone, callerAdministratorThroughDash, callerAdministratorWithLoginCookie} {
		for _, path := range []string{"/mailstack/api/accounts", "/mailstack/api/messages?account=desk",
			test.metadataPath(test.linkedMessageID), test.metadataPath(test.unlinkedMessageID), "/mailstack/api/ingress/receipts"} {
			if response := test.call(t, caller, "GET", path, ""); response.Code != http.StatusOK {
				t.Errorf("%s: GET %s = %d %s", caller.name, path, response.Code, response.Body)
			}
		}
	}
}

func TestMailstackNeedsACaller(t *testing.T) {
	test := newMailstackTestServer(t)
	if response := test.call(t, callerWithNoCredential, "GET", "/mailstack/api/accounts", ""); response.Code != http.StatusUnauthorized {
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
	for _, path := range []string{test.metadataPath(test.unlinkedMessageID), "/mailstack/api/accounts"} {
		if response := test.call(t, forging, "GET", path, ""); response.Code != http.StatusUnauthorized {
			t.Errorf("%s: GET %s = %d %s, want 401", forging.name, path, response.Code, response.Body)
		}
	}
	for _, caller := range []accessTestCaller{callerMemberThroughDash, callerMemberWithLoginCookie} {
		for path, want := range map[string]int{
			test.metadataPath(test.linkedMessageID):   http.StatusOK,
			test.metadataPath(test.unlinkedMessageID): http.StatusNotFound,
			"/mailstack/api/messages?account=desk":    http.StatusForbidden,
			"/mailstack/api/ingress/receipts":         http.StatusForbidden,
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
		response := test.call(t, caller, "GET", "/mailstack/api/accounts", "")
		if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || response.Code != http.StatusOK || len(listed.Accounts) != 0 {
			t.Errorf("%s: the account list of a member with no grant = %d %s", caller.name, response.Code, response.Body)
		}
	}

	// A can_read_mail grant on desk, checked against mailstack in this
	// process, opens the account.
	ctx := context.Background()
	checker := test.server.grantResourceChecker(test.stores.Principals)
	if _, _, err := test.stores.Grants.Create(ctx, test.grantsDirectory(), checker, grantstore.GrantRequest{
		PrincipalID: accessTestMemberID, Relation: grantstore.RelationCanReadMail, ResourceType: grantstore.ResourceTypeMailAccount, ResourceID: "nowhere",
	}); !errors.Is(err, grantstore.ErrInvalidGrant) || !strings.Contains(err.Error(), "does not exist in mailstack") {
		t.Fatalf("a grant on an account mailstack does not have: %v, want it refused as one mailstack does not have", err)
	}
	if _, _, err := test.stores.Grants.Create(ctx, test.grantsDirectory(), checker, grantstore.GrantRequest{
		PrincipalID: accessTestMemberID, Relation: grantstore.RelationCanReadMail, ResourceType: grantstore.ResourceTypeMailAccount, ResourceID: "desk",
	}); err != nil {
		t.Fatalf("grant can_read_mail on desk: %v", err)
	}
	for _, path := range []string{test.metadataPath(test.unlinkedMessageID), "/mailstack/api/messages?account=desk"} {
		if response := test.call(t, callerMemberWithLoginCookie, "GET", path, ""); response.Code != http.StatusOK {
			t.Errorf("with can_read_mail: GET %s = %d %s", path, response.Code, response.Body)
		}
	}
	// Reading is not importing.
	if response := test.call(t, callerMemberWithLoginCookie, "GET", "/mailstack/api/accounts/desk/import-sources", ""); response.Code != http.StatusForbidden {
		t.Errorf("import sources with can_read_mail only = %d %s, want 403", response.Code, response.Body)
	}
}

// Pushed delivery takes no login: the request reaches mailstack, which
// answers for the ingress credential.
func TestMailstackTakesAPushedDeliveryWithAnIngressCredentialAndNoLogin(t *testing.T) {
	test := newMailstackTestServer(t)
	path := "/mailstack/api/ingress/deliveries"
	body := `{"deliveries":[{"delivery_id":"d1","account_id":"desk","message":{"provider_message_id":"SRC-9","source_revision":"r1",
		"subject":"Pushed","from":{"email":"pat@demo.invalid"},"received_at":"2026-10-02T09:30:00Z","body_text":"hello"}}]}`
	response := test.call(t, callerWithNoCredential, "POST", path, body)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), mailstack.IngressTokenHeader) {
		t.Fatalf("no ingress token: %d %s, want mailstack's 401 naming its header", response.Code, response.Body)
	}

	minted := test.call(t, callerServiceTokenAlone, "POST", "/mailstack/api/ingress/credentials", `{"source":"custody","account_ids":["desk"]}`)
	var credential struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &credential); err != nil || credential.Token == "" {
		t.Fatalf("mint a credential: %d %s", minted.Code, minted.Body)
	}
	delivering := accessTestCaller{"a sender with an ingress credential", func(_ *accessTestServer, request *http.Request) {
		request.Header.Set(mailstack.IngressTokenHeader, credential.Token)
	}}
	response = test.call(t, delivering, "POST", path, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"accepted"`) {
		t.Fatalf("delivery: %d %s", response.Code, response.Body)
	}
	// The credential opens nothing else.
	if response := test.call(t, delivering, "GET", "/mailstack/api/accounts", ""); response.Code != http.StatusUnauthorized {
		t.Errorf("the ingress credential on another route = %d %s, want 401", response.Code, response.Body)
	}
}

// Triage's sender lookup reads mailstack in this process.
func TestTriageReadsAMessagesSenderFromMailstackInProcess(t *testing.T) {
	test := newMailstackTestServer(t)
	headers, err := test.server.mailstackClient.GetMessageHeaders(context.Background(), "desk", test.linkedMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if headers.From.Email != "pat@demo.invalid" || headers.Subject != "Printer jammed again" {
		t.Errorf("headers %+v", headers)
	}
}

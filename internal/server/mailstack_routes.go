package server

// mailstack, run in this process (internal/mail). Its routes stay
// where the identity-carrying proxy put them: /mail/<rest> answers
// mailstack's /<rest>.
//
// The standalone service took one bearer token from every caller and
// trusted X-Principal-Id beside it. In this process mailstack reads its
// caller from the request context, set here from the credential request
// authorization verified, so no header a client sends can name the caller,
// and the bearer retires. The internal service — this server's own service
// token, naming nobody — is unrestricted, as the bearer's holder was; a
// principal is narrowed to its mail grants and the messages its tickets
// link, read from principal-store, grant-store and kanban-store in this
// process. Pushed delivery is the one route that takes neither: a sender
// presents an ingress credential, which mailstack checks itself.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/login"

	"github.com/kayushkin/llm-bridge-server/internal/httproutes"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
	mailfilestore "github.com/kayushkin/llm-bridge-server/internal/mail/filestore"
)

// mailstackIngressDeliveryPattern is pushed delivery, answered by mailstack
// against the ingress credential the sender presents.
const mailstackIngressDeliveryPattern = "POST " + mailMountPrefix + "/api/ingress/deliveries"

// errMailstackOff is a use of mailstack on a server that never enabled it.
var errMailstackOff = errors.New("mailstack is off: EnableMail was never called on this server")

// EnableMail serves mailstack from mail. main calls it once, after
// EnableAccess and EnableIssues and before the server listens; until
// then, and in a test that never calls it, /mail/ answers 503 and
// triage's sender lookup is off.
func (s *Server) EnableMail(mail *mail.Server) {
	s.mailstack.Store(mail)
	s.mailMessages = mail
}

// MailstackPeopleDirectory is what mailstack reads a person's access from:
// principal-store and grant-store in this process, and kanban-store once
// EnableIssues has run.
func (s *Server) MailstackPeopleDirectory(stores *access.Stores) mail.PeopleDirectory {
	return mailstackPeopleDirectory{server: s, principals: stores.Principals, grants: stores.Grants,
		directory: access.GrantsPrincipalDirectory(stores.Principals)}
}

// MailstackFileStore is file-store as mailstack keeps attachment bytes: the
// module in this process, called in Go. Until EnableFiles each call fails,
// and mailstack refuses attachments.
func (s *Server) MailstackFileStore() mailfilestore.Files {
	return mailstackFileStore{server: s}
}

// serveMailstack answers /mail/<rest> with mailstack's /<rest>, as
// caller.
func (s *Server) serveMailstack(w http.ResponseWriter, r *http.Request, caller *mail.Caller) {
	mailServer := s.mailstack.Load()
	if mailServer == nil {
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "mailstack_off", errMailstackOff.Error())
		return
	}
	storePath, err := url.Parse(httproutes.EscapedPathAfterPrefix(r.URL, mailMountPrefix))
	if err != nil {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_path", fmt.Sprintf("the path after %s/ is not one mailstack can read: %v", mailMountPrefix, err))
		return
	}
	ctx := r.Context()
	if caller != nil {
		ctx = mail.ContextWithCaller(ctx, *caller)
	}
	served := r.Clone(ctx)
	served.URL.Path = storePath.Path
	served.URL.RawPath = storePath.RawPath
	served.RequestURI = served.URL.RequestURI()
	// mailstack reads none of these, and none is its business: the caller is
	// in the context.
	for key := range served.Header {
		for _, removed := range requestHeadersNeverForwardedToStores {
			if strings.EqualFold(key, removed) {
				delete(served.Header, key)
			}
		}
	}
	login.RemoveCookieFromRequestHeader(served.Header, login.LoginSessionCookieName)
	mailServer.Handler().ServeHTTP(w, served)
}

// handleMailstackAsCaller answers /mail/<rest> as the verified caller.
func (s *Server) handleMailstackAsCaller(w http.ResponseWriter, r *http.Request) {
	var caller mail.Caller
	if principalID, identified := requestcaller.PrincipalIdentityOfRequest(r); identified {
		caller = mail.Caller{PrincipalID: principalID}
	} else if s.requestIsTheInternalServiceActingAsItself(r) {
		caller = mail.Caller{InternalService: true}
	} else {
		// Request authorization lets nothing else through to a store route.
		httpjson.WriteJSONError(w, http.StatusUnauthorized, "mailstack_requires_a_caller",
			mailMountPrefix+"/ answers a principal or this server's service token")
		return
	}
	s.serveMailstack(w, r, &caller)
}

// handleMailstackIngressDelivery answers pushed delivery with no caller:
// mailstack checks the X-Mailstack-Ingress-Token the sender presents against
// its ingress credentials, and that credential delivers into its accounts
// and does nothing else.
func (s *Server) handleMailstackIngressDelivery(w http.ResponseWriter, r *http.Request) {
	s.serveMailstack(w, r, nil)
}

// checkMailAccountExists is grant-store's check that a mail_account grant
// names an account mailstack has.
func (s *Server) checkMailAccountExists(accountID string) error {
	mail := s.mailstack.Load()
	if mail == nil {
		return fmt.Errorf("%w, so mail account %q cannot be checked", errMailstackOff, accountID)
	}
	exists, err := mail.AccountExists(accountID)
	if err != nil {
		return fmt.Errorf("mailstack: reading account %s: %w", accountID, err)
	}
	if !exists {
		return fmt.Errorf("%w: mailstack has no account %q", grants.ErrResourceNotFound, accountID)
	}
	return nil
}

// mailstackPeopleDirectory answers mailstack's questions about a person from
// the stores in this process, as mailstack asked the standalone stores over
// HTTP with their service tokens.
type mailstackPeopleDirectory struct {
	server     *Server
	principals *principals.Store
	grants     *grants.Store
	directory  grants.PrincipalDirectory
}

func (d mailstackPeopleDirectory) Principal(ctx context.Context, principalID string) (mail.PrincipalFacts, error) {
	principal, err := d.principals.Get(ctx, principalID, false)
	if errors.Is(err, principals.ErrNotFound) {
		return mail.PrincipalFacts{}, mail.ErrPrincipalUnknown
	}
	if err != nil {
		return mail.PrincipalFacts{}, fmt.Errorf("reading %s: %w", principalID, err)
	}
	return mail.PrincipalFacts{Kind: principal.Kind, Disabled: principal.DisabledAt != 0, IsAdministrator: principal.IsAdministrator}, nil
}

func (d mailstackPeopleDirectory) MailGrants(ctx context.Context, principalID string) ([]mail.MailGrant, error) {
	effective, err := d.grants.Effective(ctx, d.directory, principalID, "", grants.ResourceTypeMailAccount)
	if errors.Is(err, grants.ErrNotFound) {
		return nil, mail.ErrPrincipalUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("the mail grants of %s: %w", principalID, err)
	}
	held := make([]mail.MailGrant, 0, len(effective))
	for _, grant := range effective {
		held = append(held, mail.MailGrant{Relation: grant.Relation, AccountID: grant.ResourceID})
	}
	return held, nil
}

func (d mailstackPeopleDirectory) MessagesVisibleThroughTickets(ctx context.Context, principalID string, locators []string) ([]bool, error) {
	handlers := d.server.kanbanStore.Load()
	if handlers == nil || handlers.api == nil {
		return nil, fmt.Errorf("%w, so no ticket can show a message", errKanbanStoreOff)
	}
	return handlers.api.EntitiesVisibleToPrincipal(principalID, mailstackEmailEntityType, locators)
}

// mailstackEmailEntityType is the kanban entity type a card links a message
// by, its ref the message's locator "<account_id>:<id>": reference-store's
// email type.
const mailstackEmailEntityType = "email"

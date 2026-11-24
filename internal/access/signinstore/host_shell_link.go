package signinstore

import (
	"context"
	"fmt"
	"time"
)

// MakeSignInLinkFromHostShell makes a one-time link for a principal, for
// someone with a shell on the host the server runs on. It is the way back in
// when nobody can sign in any other way, so it needs nothing but the database
// and principal-store: no running server and no credential. Having the shell,
// and with it the database URL, is the credential.
//
// Only an enabled human gets a link. It returns the link and the address to
// open, which holds the secret this once.
func MakeSignInLinkFromHostShell(ctx context.Context, store *Store, directory *Directory, principalID, signInPageURL string) (SignInLink, string, error) {
	principal, err := directory.Principal(ctx, principalID)
	if err != nil {
		return SignInLink{}, "", err
	}
	if err := principal.refusalToSignIn(); err != nil {
		return SignInLink{}, "", err
	}
	link, secret, err := store.MakeSignInLink(ctx, principal.ID, SentByHostShell, "")
	if err != nil {
		return SignInLink{}, "", err
	}
	return link, signInPageURL + "#" + secret, nil
}

// HostShellLinkMessage is what the command prints.
func HostShellLinkMessage(link SignInLink, url string) string {
	return fmt.Sprintf("%s for %s, works once until %s:\n%s\n", link.ID, link.PrincipalID,
		time.Unix(link.ExpiresAt, 0).UTC().Format(time.RFC3339), url)
}

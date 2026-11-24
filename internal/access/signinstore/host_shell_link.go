package signinstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MakeSignInLinkFromHostShell is `sign-in-store make-link <principal_id>`: a
// one-time link for a principal, made by someone with a shell on this host.
// It is the way back in when nobody can sign in any other way, so it needs
// nothing but the database and principal-store: no running service, no
// service token. Having the shell is the credential.
//
// It refuses to create a database: a missing one means the command is running
// as the wrong user or with the wrong data directory, and a link in a fresh
// database would sign nobody in.
func MakeSignInLinkFromHostShell(ctx context.Context, dataDir string, directory *Directory, principalID, signInPageURL string) (SignInLink, string, error) {
	databasePath := filepath.Join(dataDir, "sign-in-store.db")
	if _, err := os.Stat(databasePath); err != nil {
		return SignInLink{}, "", fmt.Errorf("%s: %w; run this as the user the service runs as (sudo -iu kayushkincom)", databasePath, err)
	}
	principal, err := directory.Principal(ctx, principalID)
	if err != nil {
		return SignInLink{}, "", err
	}
	if err := principal.refusalToSignIn(); err != nil {
		return SignInLink{}, "", err
	}
	store, err := Open(dataDir)
	if err != nil {
		return SignInLink{}, "", err
	}
	defer store.Close()
	link, secret, err := store.MakeSignInLink(principal.ID, SentByHostShell, "")
	if err != nil {
		return SignInLink{}, "", err
	}
	return link, signInPageURL + "#" + secret, nil
}

// ErrUsage is a command line the command does not understand.
var ErrUsage = errors.New("usage: sign-in-store make-link <principal_id>")

// HostShellLinkMessage is what the command prints.
func HostShellLinkMessage(link SignInLink, url string) string {
	return fmt.Sprintf("%s for %s, works once until %s:\n%s\n", link.ID, link.PrincipalID,
		time.Unix(link.ExpiresAt, 0).UTC().Format(time.RFC3339), url)
}

package signinstore

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAHostShellLinkSignsInAndNeedsAnExistingDatabase(t *testing.T) {
	service := newTestService(t)
	link, url, err := MakeSignInLinkFromHostShell(context.Background(), service.store.DataDir(), service.directory, testAdministrator, "https://kayushkin.com/signin")
	if err != nil {
		t.Fatal(err)
	}
	if link.SentBy != SentByHostShell || !strings.HasPrefix(url, "https://kayushkin.com/signin#") {
		t.Fatalf("link %+v url %s", link, url)
	}
	redeemed := service.do(t, "POST", "/redeem", map[string]string{"secret": strings.TrimPrefix(url, "https://kayushkin.com/signin#")}, nil, nil)
	if redeemed.Code != http.StatusOK {
		t.Fatalf("redeeming a host-shell link = %d %s", redeemed.Code, redeemed.Body)
	}
	if _, _, err := MakeSignInLinkFromHostShell(context.Background(), t.TempDir(), service.directory, testAdministrator, "https://kayushkin.com/signin"); err == nil {
		t.Fatal("a data directory with no database must be refused, not given a fresh one")
	}
	if _, _, err := MakeSignInLinkFromHostShell(context.Background(), service.store.DataDir(), service.directory, testGroup, "https://kayushkin.com/signin"); err == nil {
		t.Fatal("a group cannot sign in")
	}
}

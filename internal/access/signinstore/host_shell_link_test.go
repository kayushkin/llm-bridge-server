package signinstore

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAHostShellLinkSignsInAndOnlyForAnEnabledHuman(t *testing.T) {
	service := newTestService(t)
	link, url, err := MakeSignInLinkFromHostShell(context.Background(), service.store, service.directory, testAdministrator, "https://kayushkin.com/signin")
	if err != nil {
		t.Fatal(err)
	}
	if link.SentBy != SentByHostShell || link.MadeBy != "" || !strings.HasPrefix(url, "https://kayushkin.com/signin#") {
		t.Fatalf("link %+v url %s", link, url)
	}
	redeemed := service.do(t, "POST", "/redeem", map[string]string{"secret": strings.TrimPrefix(url, "https://kayushkin.com/signin#")}, nil, nil)
	if redeemed.Code != http.StatusOK {
		t.Fatalf("redeeming a host-shell link = %d %s", redeemed.Code, redeemed.Body)
	}
	if _, _, err := MakeSignInLinkFromHostShell(context.Background(), service.store, service.directory, testGroup, "https://kayushkin.com/signin"); err == nil {
		t.Fatal("a group cannot sign in")
	}
	if _, _, err := MakeSignInLinkFromHostShell(context.Background(), service.store, service.directory, "principal_000999", "https://kayushkin.com/signin"); err == nil {
		t.Fatal("a principal principal-store does not have cannot sign in")
	}
	service.world.disablePrincipal(testPerson, 1790000000)
	if _, _, err := MakeSignInLinkFromHostShell(context.Background(), service.store, service.directory, testPerson, "https://kayushkin.com/signin"); err == nil {
		t.Fatal("a disabled person cannot sign in")
	}
}

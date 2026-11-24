package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	signinstore "github.com/kayushkin/llm-bridge-server/internal/access/signinstore"
)

// runCommand runs a command given on the command line instead of the
// server: today only make-link, the way back in from a shell on this host.
func runCommand(settings *servicesettings.Registry, arguments []string) int {
	if len(arguments) != 2 || arguments[0] != "make-link" {
		fmt.Fprintln(os.Stderr, signinstore.ErrUsage)
		return 2
	}
	directory := signinstore.NewDirectory(signinstore.DirectoryConfig{PrincipalStoreURL: settings.String(signinstore.SettingPrincipalStoreURL)})
	link, url, err := signinstore.MakeSignInLinkFromHostShell(context.Background(), settings.String(signinstore.SettingDataDirectory),
		directory, arguments[1], settings.String(signinstore.SettingSignInPageURL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "make-link: %v\n", err)
		return 1
	}
	fmt.Print(signinstore.HostShellLinkMessage(link, url))
	return 0
}

func main() {
	settings, err := signinstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if len(os.Args) > 1 {
		os.Exit(runCommand(settings, os.Args[1:]))
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(signinstore.SettingListenAddress)

	store, err := signinstore.Open(settings.String(signinstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	directory := signinstore.NewDirectory(signinstore.DirectoryConfig{
		PrincipalStoreURL:     settings.String(signinstore.SettingPrincipalStoreURL),
		PeopleStoreURL:        settings.String(signinstore.SettingPeopleStoreURL),
		MultichatURL:          settings.String(signinstore.SettingMultichatURL),
		AuthStoreURL:          settings.String(signinstore.SettingAuthStoreURL),
		AuthStoreToken:        settings.String(signinstore.SettingAuthStoreToken),
		MultichatCredentialID: settings.String(signinstore.SettingMultichatCredential),
	})

	mux := http.NewServeMux()
	signinstore.RegisterHandlers(mux, store, directory, signinstore.ServerConfig{
		ServiceToken:  settings.String(signinstore.SettingServiceToken),
		SignInPageURL: settings.String(signinstore.SettingSignInPageURL),
		CookieDomain:  settings.String(signinstore.SettingCookieDomain),
		Discord: signinstore.DiscordConfig{
			ApplicationID:            settings.String(signinstore.SettingDiscordApplicationID),
			ClientSecretCredentialID: settings.String(signinstore.SettingDiscordSecretID),
			RedirectURL:              settings.String(signinstore.SettingDiscordRedirectURL),
		},
	})
	signinstore.RegisterSettingsHandler(mux, settings)

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("sign-in-store listening on %s (data=%s)", addr, store.DataDir())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

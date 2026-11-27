package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/apiauth"
	"github.com/kayushkin/llm-bridge-server/internal/mail/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

func openMailModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := mailmodule.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableMail opens mailstack's module in Postgres and serves mailstack from
// srv at /mailstack/. It reads a person's access from the access stores and
// kanban-store in this process, and keeps attachment bytes in the file-store
// module in this process, so it runs after both are enabled. Gmail, Outlook
// and IMAP accounts reach their providers only when
// mail.provider_connections.enabled is on; nothing here runs on its own
// — mailstack fetches mail only when a request asks for it, and sends none.
// The returned function closes the pool.
func enableMail(ctx context.Context, cfg *config.Config, srv *server.Server, accessStores *access.Stores) (func(), error) {
	providerConnections := cfg.Settings.Boolean(config.SettingMailstackProviderConnectionsEnabled)
	if providerConnections && cfg.AuthStoreToken == "" {
		return nil, fmt.Errorf("mailstack: %s is on but auth_store.token is not set: Gmail and Outlook tokens are read from auth-store", config.SettingMailstackProviderConnectionsEnabled)
	}
	pool, err := openMailModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("mailstack: %w", err)
	}
	db := mailmodule.New(pool)
	parts := mail.ServerParts{
		Accounts:                   accounts.New(db),
		LocalMail:                  localmail.New(db),
		ProviderConnectionsEnabled: providerConnections,
		Files:                      srv.MailstackFileStore(),
		People:                     srv.MailstackPeopleDirectory(accessStores),
	}
	if providerConnections {
		parts.Tokens = apiauth.New(cfg.AuthStoreURL, cfg.AuthStoreToken)
	}
	mail := mail.NewServer(parts)
	srv.EnableMailstack(mail)
	if providerConnections {
		log.Printf("postgres: mailstack open, served at /mailstack/; Gmail, Outlook and IMAP accounts reach their providers, with tokens from auth-store at %s", cfg.AuthStoreURL)
	} else {
		log.Printf("postgres: mailstack open, served at /mailstack/; only local accounts answer, since %s is off and no mail provider is reached", config.SettingMailstackProviderConnectionsEnabled)
	}
	return func() {
		if err := mail.Shutdown(context.Background()); err != nil {
			log.Printf("mailstack: closing IMAP connections: %v", err)
		}
		if err := pool.Close(); err != nil {
			log.Printf("postgres: closing the mail module: %v", err)
		}
	}, nil
}

// importMailstackCmd copies a standalone mailstack's two SQLite files into
// the mail module, keeping every row, id and time, and prints what it copied.
// It refuses a module that already holds any row. Attachment bytes are
// file-store's, and import-file-store carries them.
func importMailstackCmd(args []string) {
	flags := flag.NewFlagSet("import-mailstack", flag.ExitOnError)
	accountsPath := flags.String("accounts-sqlite", "", "the standalone mailstack's accounts.db to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	localMailPath := flags.String("local-mail-sqlite", "", "its local-mail.db, copied the same way; leave it out when the store never had a local account and so never made the file")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *accountsPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-mailstack -accounts-sqlite <file> [-local-mail-sqlite <file>]")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := openMailModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer pool.Close()
	counts, err := mailmodule.ImportStandaloneDatabases(ctx, pool, *accountsPath, *localMailPath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	for _, line := range []struct {
		name  string
		count int64
	}{
		{"accounts", counts.Accounts}, {"imap_creds", counts.IMAPLogins}, {"imap_folder_map", counts.IMAPFolderMaps},
		{"messages", counts.IndexedMessages}, {"message_bodies", counts.RecordedBodies},
		{"local_messages", counts.LocalMessages}, {"local_message_bodies", counts.LocalMessageBodies},
		{"local_import_sources", counts.LocalImportSources}, {"local_message_revisions", counts.LocalMessageRevisions},
		{"local_message_attachments", counts.LocalMessageAttachments}, {"ingress_credentials", counts.IngressCredentials},
		{"ingress_credential_accounts", counts.IngressCredentialAccounts}, {"delivery_receipts", counts.DeliveryReceipts},
		{"message_purges", counts.MessagePurges}, {"message_events", counts.MessageEvents},
	} {
		fmt.Printf("%-28s %d\n", line.name+":", line.count)
	}
	fmt.Printf("numbering goes on after: message_bodies %d, delivery_receipts %d, message_events %d\n",
		counts.LastRecordedBody, counts.LastReceiptID, counts.LastEventSequence)
	repaired := make([]string, 0, len(counts.RepairedText))
	for column := range counts.RepairedText {
		repaired = append(repaired, column)
	}
	sort.Strings(repaired)
	for _, column := range repaired {
		fmt.Printf("held with U+FFFD for bytes that were not UTF-8: %s, %d values\n", column, counts.RepairedText[column])
	}
}

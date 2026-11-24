package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictionstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// openPredictionStoreModule makes the prediction-store module's role and
// schema on the Postgres at url if they are missing, brings its schema up to
// this binary's migrations, and opens its pool. url names a login that may
// create roles and schemas; each step that fails is the error, and nothing
// carries on without it.
func openPredictionStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := predictionstore.Module()
	if err != nil {
		return nil, err
	}
	administrator, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("the Postgres URL: %w", err)
	}
	defer administrator.Close()
	if err := administrator.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("reaching Postgres: %w", err)
	}
	if err := postgresstorage.Provision(ctx, administrator, module); err != nil {
		return nil, err
	}
	db, err := postgresstorage.Open(ctx, url, module)
	if err != nil {
		return nil, err
	}
	report, err := postgresstorage.Migrate(ctx, db, module)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, migration := range report.Applied {
		log.Printf("postgres: module %s: applied migration %d (%s)", module.Schema, migration.Version, migration.Name)
	}
	if len(report.NewerThanThisBinary) > 0 {
		log.Printf("postgres: module %s: the database has migrations %v that this binary does not know; a newer server migrated it", module.Schema, report.NewerThanThisBinary)
	}
	return db, nil
}

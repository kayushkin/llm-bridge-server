// Package access is who may sign in and what they may use: principal-store,
// grant-store, sign-in-store and permission-store, run inside the bridge.
//
// Each store keeps its records in a Postgres module of its own (its schema and
// role), so one store reaches another's records by calling its code, never its
// tables. This package opens the four modules and joins the stores to each
// other in process, where the standalone stores called each other over HTTP:
// grant-store and sign-in-store read principals from principal-store's code.
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	permstore "github.com/kayushkin/llm-bridge-server/internal/access/permissionstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/signinstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// Stores is the four access stores, open on their modules.
type Stores struct {
	Principals  *principalstore.Store
	Grants      *grantstore.Store
	SignIn      *signinstore.Store
	Permissions *permstore.Store
	// PermissionEngine is the one rule engine: the mounted rule routes
	// invalidate its cache, and the permission prehook asks it.
	PermissionEngine *permstore.Engine

	pools []*sql.DB
}

// Close closes every module's pool.
func (stores *Stores) Close() error {
	var errs []error
	for _, pool := range stores.pools {
		errs = append(errs, pool.Close())
	}
	return errors.Join(errs...)
}

// Modules is every module access keeps records in, in the order they are
// opened.
func Modules() ([]postgresstorage.Module, error) {
	var modules []postgresstorage.Module
	for _, module := range []func() (postgresstorage.Module, error){
		principalstore.Module, grantstore.Module, signinstore.Module, permstore.Module,
	} {
		described, err := module()
		if err != nil {
			return nil, err
		}
		modules = append(modules, described)
	}
	return modules, nil
}

// OpenStores makes each module's role and schema on the Postgres at url if
// they are missing, brings each schema up to this binary's migrations, and
// opens the stores. url names a login that may create roles and schemas. Any
// step that fails is the error, and every pool opened before it is closed.
func OpenStores(ctx context.Context, url string) (*Stores, error) {
	modules, err := Modules()
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
	stores := &Stores{}
	pools := map[string]*sql.DB{}
	for _, module := range modules {
		pool, err := provisionMigrateAndOpen(ctx, administrator, url, module)
		if err != nil {
			stores.Close()
			return nil, err
		}
		stores.pools = append(stores.pools, pool)
		pools[module.Schema] = pool
	}
	principalsModule, grantsModule, signInModule, permissionsModule := modules[0], modules[1], modules[2], modules[3]
	stores.Principals = principalstore.New(pools[principalsModule.Schema])
	stores.Grants = grantstore.New(pools[grantsModule.Schema])
	stores.SignIn = signinstore.New(pools[signInModule.Schema])
	stores.Permissions = permstore.New(pools[permissionsModule.Schema])
	stores.PermissionEngine = permstore.NewEngine(stores.Permissions)
	return stores, nil
}

// OpenModule provisions, migrates and opens one module, for a command that
// needs one store alone (an import).
func OpenModule(ctx context.Context, url string, module postgresstorage.Module) (*sql.DB, error) {
	administrator, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("the Postgres URL: %w", err)
	}
	defer administrator.Close()
	if err := administrator.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("reaching Postgres: %w", err)
	}
	return provisionMigrateAndOpen(ctx, administrator, url, module)
}

func provisionMigrateAndOpen(ctx context.Context, administrator *sql.DB, url string, module postgresstorage.Module) (*sql.DB, error) {
	provisioned, err := postgresstorage.Provision(ctx, administrator, module)
	if err != nil {
		return nil, err
	}
	if former := provisioned.RenamedFrom; former != nil {
		log.Printf("postgres: module %s: renamed schema %s and role %s, its names before, to %s and %s", module.Schema, former.Schema, former.Role, module.Schema, module.Role)
	}
	pool, err := postgresstorage.Open(ctx, url, module)
	if err != nil {
		return nil, err
	}
	report, err := postgresstorage.Migrate(ctx, pool, module)
	if err != nil {
		pool.Close()
		return nil, err
	}
	for _, migration := range report.Applied {
		log.Printf("postgres: module %s: applied migration %d (%s)", module.Schema, migration.Version, migration.Name)
	}
	if len(report.NewerThanThisBinary) > 0 {
		log.Printf("postgres: module %s: the database has migrations %v that this binary does not know; a newer server migrated it", module.Schema, report.NewerThanThisBinary)
	}
	return pool, nil
}

// StoresOnPools builds the stores over pools a caller already opened, one per
// module in Modules order: what a test does with postgrestest databases.
func StoresOnPools(principals, grants, signIn, permissions *sql.DB) *Stores {
	stores := &Stores{
		Principals:  principalstore.New(principals),
		Grants:      grantstore.New(grants),
		SignIn:      signinstore.New(signIn),
		Permissions: permstore.New(permissions),
	}
	stores.PermissionEngine = permstore.NewEngine(stores.Permissions)
	return stores
}

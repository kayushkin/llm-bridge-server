// Package mailmoduletest gives a test a mail module of its own, in a
// postgrestest database that is dropped when the test ends.
package mailmoduletest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewDB is a migrated, empty mail module for one test.
func NewDB(t testing.TB) *mailmodule.DB {
	t.Helper()
	module, err := mailmodule.Module()
	if err != nil {
		t.Fatalf("the mail module: %v", err)
	}
	return mailmodule.New(postgrestest.NewDatabase(t, module).Open(module))
}

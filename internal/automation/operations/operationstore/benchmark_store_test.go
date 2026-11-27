package operationstore_test

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore/operationstoretest"
)

func openBenchmarkStore(b *testing.B) *operationstore.Store {
	return operationstoretest.NewStore(b)
}

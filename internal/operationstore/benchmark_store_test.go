package operationstore_test

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore/operationstoretest"
)

func openBenchmarkStore(b *testing.B) *operationstore.Store {
	return operationstoretest.NewStore(b)
}

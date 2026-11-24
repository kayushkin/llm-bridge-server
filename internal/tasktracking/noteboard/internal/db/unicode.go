package db

import (
	"database/sql/driver"
	"fmt"
	"modernc.org/sqlite"
	"strings"
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("unicode_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("unicode_lower requires text")
		}
		return strings.ToLower(value), nil
	})
}

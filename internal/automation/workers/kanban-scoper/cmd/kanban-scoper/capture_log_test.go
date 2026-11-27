package main

import (
	"bytes"
	"log"
	"testing"
)

// captureLog collects everything the run under it writes to the standard logger,
// which is where degradations.Report lands and therefore the only place a person
// reading scheduler run history sees a shortfall.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	priorOut, priorFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(priorOut); log.SetFlags(priorFlags) })
	fn()
	return buf.String()
}

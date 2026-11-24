package modelstore

import (
	"testing"

	_ "modernc.org/sqlite"
)

// TestSeedRerunResetsEveryUserSetField pins what Seed's doc comment claims, so
// the comment cannot quietly become false again. Seed is documented as unsafe to
// re-run on a configured store, and this is the measurement behind that sentence:
// a short name, a priority and an enabled flag that a person set are all gone
// after a second Seed.
//
// This test asserts the CURRENT behavior, which is a fault the comment warns
// about rather than one it defends. If Seed is ever made non-destructive — the
// obvious repair is to read the existing row and call adoptUserSetFieldsFrom, the
// way every provider sync does — this test is what tells whoever does it to
// rewrite the comment in the same change, instead of leaving a warning that no
// longer describes the code.
func TestSeedRerunResetsEveryUserSetField(t *testing.T) {
	const model = "claude-opus-4-6"

	s := tempStore(t)
	if err := s.Seed(); err != nil {
		t.Fatalf("first Seed: %v", err)
	}

	if err := s.SetShortName(model, "mine"); err != nil {
		t.Fatalf("SetShortName: %v", err)
	}
	if err := s.SetPriority(model, 1); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if err := s.SetEnabled(model, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	configured, err := s.ResolveModel(model)
	if err != nil {
		t.Fatalf("ResolveModel before re-seed: %v", err)
	}
	if configured.ShortName != "mine" || configured.Priority != 1 || configured.Enabled {
		t.Fatalf("the setters did not take, so the re-seed below would prove nothing: %+v", configured)
	}

	if err := s.Seed(); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	after, err := s.ResolveModel(model)
	if err != nil {
		t.Fatalf("ResolveModel after re-seed: %v", err)
	}
	if after.ShortName == "mine" || after.Priority == 1 || !after.Enabled {
		t.Errorf("Seed no longer overwrites user-set fields — good, but its doc comment still "+
			"says it does. Update the comment on Seed in the same change as the behavior.\n"+
			"got %+v", after)
	}
}

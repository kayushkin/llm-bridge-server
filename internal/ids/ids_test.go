package ids

import "testing"

func TestIsServerIDTakesOnlyThePrefixAndAULID(t *testing.T) {
	made := NewServerID()
	if !IsServerID(made) {
		t.Fatalf("IsServerID(%q) = false for an id NewServerID made", made)
	}
	for _, value := range []string{
		"",
		"server_",
		"server_000042",
		made[len("server_"):],
		"msg_" + made[len("server_"):],
		made + "0",
		made + " ",
	} {
		if IsServerID(value) {
			t.Errorf("IsServerID(%q) = true, want false", value)
		}
	}
}

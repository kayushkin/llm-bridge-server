package harness

import "github.com/kayushkin/llm-bridge/msg"

// deriveWithoutStatus is derive() minus the session_status event it appends.
//
// The state-machine tests in this package assert on exactly which transition,
// usage and turn events one source event produces, by count and by index. A
// session_status rides along with most of them, and says nothing those tests
// are about; derivation_status_test.go is where it is asserted on.
func deriveWithoutStatus(d *derivationState, ev *msg.Event) []msg.Event {
	var out []msg.Event
	for _, derived := range d.derive(ev) {
		if derived.Type != msg.EventSessionStatus {
			out = append(out, derived)
		}
	}
	return out
}

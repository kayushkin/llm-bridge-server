package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
)

// Go randomises map order, so a pick that depends on it shows up only as a
// different answer on some calls. Asking many times makes that visible: with two
// candidates, a map-order pick returns both within a few calls.
const mapOrderTries = 200

// A session's events only grow, so the signature with more events is newer. The
// counts differ in width on purpose: "9" sorts above "10" as a string.
func TestTheNewestOfTwoSignaturesIsTheOneWithMoreEvents(t *testing.T) {
	older := buildSig("e_1", 9)
	newer := buildSig("e_2", 10)
	tags := map[string]bool{older: true, newer: true, "some-other-tag": true}
	for try := 0; try < mapOrderTries; try++ {
		sig, found := priorSigTag(tags)
		if sig != newer {
			t.Fatalf("try %d: priorSigTag = %q, want %q", try, sig, newer)
		}
		if found != 2 {
			t.Fatalf("try %d: found %d signatures, want 2", try, found)
		}
	}
	if n, known := priorEventCount(tags); !known || n != 10 {
		t.Errorf("priorEventCount = %d %v, want the newest signature's 10", n, known)
	}
}

// A signature from before event counts has nothing to order it by, so any
// signature that has a count is newer.
func TestASignatureWithNoEventCountIsOlderThanOneWithACount(t *testing.T) {
	current := buildSig("e_1", 3)
	tags := map[string]bool{
		"kanban-check-sig:msg=9999:state=idle:t=202609162123": true,
		current: true,
	}
	for try := 0; try < mapOrderTries; try++ {
		if sig, _ := priorSigTag(tags); sig != current {
			t.Fatalf("try %d: priorSigTag = %q, want %q", try, sig, current)
		}
	}
}

// Two signatures with the same count still give one answer every time.
func TestEqualEventCountsStillPickOneSignature(t *testing.T) {
	tags := map[string]bool{buildSig("e_a", 5): true, buildSig("e_b", 5): true}
	first, _ := priorSigTag(tags)
	for try := 0; try < mapOrderTries; try++ {
		if sig, _ := priorSigTag(tags); sig != first {
			t.Fatalf("try %d: priorSigTag = %q, then %q", try, first, sig)
		}
	}
}

func TestNoSignatureIsFoundAsNone(t *testing.T) {
	if sig, found := priorSigTag(map[string]bool{"do-not-track": true}); sig != "" || found != 0 {
		t.Errorf("priorSigTag = %q, %d, want nothing", sig, found)
	}
}

// fmt.Sscanf read "12abc" as 12 and reported success.
func TestAnEventCountWithTrailingGarbageIsUnknown(t *testing.T) {
	for _, sig := range []string{
		"kanban-check-sig:result=e_1:events=12abc",
		"kanban-check-sig:result=e_1:events=",
		"kanban-check-sig:result=e_1:events=-",
	} {
		if n, known := priorEventCount(map[string]bool{sig: true}); known {
			t.Errorf("%s: priorEventCount = %d, known; want unknown", sig, n)
		}
	}
}

// End to end: a session carrying the signature of its current response and a
// stale one left by a failed delete is skipped on every tick, and the log says
// there are two.
func TestAStaleSecondSignatureDoesNotBuyAModelCall(t *testing.T) {
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	current := buildSig(finishedResponseID, finishedEventCount)
	stale := buildSig("entry-result-0", finishedEventCount-1)
	for try := 0; try < 20; try++ {
		stub := &bridgeAndBoard{tags: []string{stale, current}}
		cfg, kc := stub.start(t)
		degraded := degradations.New("classifier")

		if ran := processSession(context.Background(), cfg, kc, testSession(), degraded); ran {
			t.Fatalf("try %d: an unchanged session was classified", try)
		}
		if stub.modelCalls != 0 {
			t.Fatalf("try %d: the model was called %d times for an unchanged session", try, stub.modelCalls)
		}
	}
	if !strings.Contains(logged.String(), "2 check signatures on one session") {
		t.Errorf("the log does not say the session carries two signatures:\n%s", logged.String())
	}
}

// A delete that fails leaves two signatures behind, and it used to be dropped
// with `_ =`.
func TestAFailedSignatureDeleteDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{tagDeleteStatus: http.StatusInternalServerError}
	_, kc := stub.start(t)
	degraded := degradations.New("classifier")

	stale := buildSig("entry-result-0", 1)
	writeSig(kc, testSession(), map[string]bool{stale: true}, buildSig(finishedResponseID, finishedEventCount), degraded)

	if !degraded.HasShortfall() {
		t.Fatal("a superseded signature that was not deleted did not register as a shortfall")
	}
	if joined := strings.Join(degraded.Reasons(), "; "); !strings.Contains(joined, "not deleted") {
		t.Errorf("the summary %q does not say the delete failed", joined)
	}
}

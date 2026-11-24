package db

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestAssignmentSnapshotRejectsConcurrentOwners(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "assignments.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err = s.AssignPrincipalToCard("card", "team", "operator"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListCardAssignments("card")
	if err != nil {
		t.Fatal(err)
	}
	version := AssignmentVersion(rows)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, person := range []string{"principal_000101", "principal_000102"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _, err := s.AssignPrincipalToCardIfVersion("card", id, "routing", version)
			results <- err
		}(person)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrAssignmentsChanged) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successes=%d", success)
	}
	after, err := s.ListCardAssignments("card")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("assignments=%v", after)
	}
}

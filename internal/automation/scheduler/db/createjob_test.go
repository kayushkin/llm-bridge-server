package db

import (
	"testing"
)

// TestEnabledOnCreateReadsAbsentAsTrue pins the one rule both creation edges
// share. Absent has to mean true or every caller written before the field
// existed would start creating dead jobs; false has to survive, or there is no
// way to create a job in the off position at all.
func TestEnabledOnCreateReadsAbsentAsTrue(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		requested *bool
		want      bool
	}{
		{"absent", nil, true},
		{"explicitly false", &no, false},
		{"explicitly true", &yes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnabledOnCreate(tc.requested); got != tc.want {
				t.Fatalf("EnabledOnCreate(%v) = %t, want %t", tc.requested, got, tc.want)
			}
		})
	}
}

// TestCreateJobFullStoresTheEnabledItIsGiven — the INSERT used to bind a
// literal 1 and the return value used to be overwritten with true, so both the
// stored row and the object handed back lied about a disabled job. Assert both:
// they failed independently.
func TestCreateJobFullStoresTheEnabledItIsGiven(t *testing.T) {
	store := patchTestStore(t)

	for _, want := range []bool{false, true} {
		created, err := store.CreateJobFull(Job{
			Name: "j", Schedule: "* * * * *", Command: "echo", Enabled: want,
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Enabled != want {
			t.Errorf("returned job has enabled = %t, want %t", created.Enabled, want)
		}
		got, err := store.GetJob(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled != want {
			t.Errorf("stored row has enabled = %t, want %t", got.Enabled, want)
		}
	}
}

// TestCreateJobHelperCreatesAnArmedJob — CreateJob takes no enabled argument,
// so it has to state its own intent. It is used by tests and by callers that
// mean "schedule this now"; if the field-less path silently produced disabled
// jobs the change that made Enabled honored would have broken them quietly.
func TestCreateJobHelperCreatesAnArmedJob(t *testing.T) {
	store := patchTestStore(t)
	created, err := store.CreateJob("j", "* * * * *", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if !created.Enabled {
		t.Fatal("CreateJob produced a disabled job")
	}
	if got, _ := store.GetJob(created.ID); !got.Enabled {
		t.Fatal("CreateJob stored a disabled row")
	}
}

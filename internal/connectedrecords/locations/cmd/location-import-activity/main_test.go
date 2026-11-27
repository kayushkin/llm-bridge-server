package main

import (
	"testing"

	activitystore "github.com/kayushkin/activity-store"
)

func TestOnlyCoordinatesFromATrustedBasisMakeAPoint(t *testing.T) {
	latitude, longitude := 10.1, 20.1
	located := func(basis string) activitystore.Activity {
		return activitystore.Activity{ID: "activity_000001", OccurredAt: 5000, Latitude: &latitude, Longitude: &longitude,
			Details: activitystore.Details{Locations: []activitystore.Location{
				{Name: "an area with no coordinates", Basis: "ignored"},
				{Name: "this general area", Latitude: &latitude, Longitude: &longitude, Basis: basis}}}}
	}
	point, reason := pointOf(located(DefaultBasis), []string{DefaultBasis})
	if reason != "" || point.SourceRef != "activity_000001" || point.Timestamp != 5000 || point.Latitude != latitude ||
		point.AccuracyMeters != nil || point.Source != "google_activity" {
		t.Errorf("a device fix = %+v, %q; want a google_activity point with no accuracy", point, reason)
	}
	if _, reason := pointOf(located("From your places (Home)"), []string{DefaultBasis}); reason == "" {
		t.Error("a saved-home location made a point")
	}
	if _, reason := pointOf(activitystore.Activity{ID: "activity_000002", OccurredAt: 5000}, []string{DefaultBasis}); reason == "" {
		t.Error("an activity with no coordinates made a point")
	}
}

package osmextract

import (
	"testing"

	"github.com/paulmach/osm"
)

// Invented addresses on a grid near 10°N 20°E.
func exampleExtract() *Extract {
	extract := &Extract{byStreetAddress: map[string][]int{}, featureGrid: map[gridCell][]int{}, addressGrid: map[gridCell][]int{}}
	address := func(ref, number, street, city string, latitude, longitude float64, more ...osm.Tag) {
		tags := osm.Tags{{Key: "addr:housenumber", Value: number}, {Key: "addr:street", Value: street}, {Key: "addr:city", Value: city}}
		extract.add(ref, append(tags, more...), latitude, longitude)
	}
	address("node/1", "12", "North Example Street", "Suburbia", 10.0601, 20.0601, osm.Tag{Key: "name", Value: "Example Cafe"},
		osm.Tag{Key: "amenity", Value: "cafe"})
	address("node/2", "12", "North Example Street", "Fartown", 11.5, 20.0)
	address("node/3", "7", "Other Avenue", "Suburbia", 10.07, 20.07)
	address("node/4", "7", "Other Avenue", "Elsewhere", 10.5, 20.5)
	return extract
}

func TestALocationIsFoundByItsStreetAddressOffline(t *testing.T) {
	extract := exampleExtract()
	for location, want := range map[string]string{
		"Some Hall, 12 N Example St, Suburbia, XX 00000": "node/1",
		"12 north example street #5, Fartown":            "node/2",
		"Room, 7 Other Ave Suite 100, Suburbia":          "node/3",
	} {
		found, ok := extract.Geocode(location, 150)
		if !ok || found.Address.Ref != want {
			t.Errorf("%q = %+v %v, want %s", location, found, ok, want)
		}
	}
	if found, _ := extract.Geocode("Some Hall, 12 N Example St, Suburbia", 150); found.Venue != "Some Hall" {
		t.Errorf("venue = %q", found.Venue)
	}
	// Two towns' 7 Other Avenue, and the location names neither: no answer.
	if found, ok := extract.Geocode("7 Other Avenue", 150); ok {
		t.Errorf("an address in two towns = %+v, want none", found)
	}
	if near := extract.FeaturesNear(10.06, 20.06, 100); len(near) != 1 || near[0].Name != "Example Cafe" || near[0].Address.Line() != "12 North Example Street, Suburbia" {
		t.Errorf("features near = %+v", near)
	}
	if address, _, ok := extract.NearestAddress(10.0699, 20.0699, 100); !ok || address.Ref != "node/3" {
		t.Errorf("nearest address = %+v %v", address, ok)
	}
}

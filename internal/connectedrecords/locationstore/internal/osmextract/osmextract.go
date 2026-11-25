// Package osmextract reads OpenStreetMap extracts (.osm.pbf, such as
// Geofabrik's regional downloads) from disk: the named features a person
// might go to and the street addresses, each at a coordinate. It never goes
// online; the extracts are public files downloaded beforehand, which sends
// nothing about anyone.
package osmextract

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"

	"github.com/kayushkin/location-store/internal/geonames"
)

// typeKeys are the tags that make a named feature somewhere a person goes,
// rather than a road or a river.
var typeKeys = []string{"amenity", "shop", "leisure", "tourism", "office", "craft", "healthcare", "sport", "club", "historic", "building"}

// keptKeys are the tags a feature keeps: its type, and what helps a person
// recognize it.
var keptKeys = append([]string{"name", "brand", "cuisine", "religion", "operator", "website", "opening_hours", "denomination"}, typeKeys...)

// Feature is a named feature.
type Feature struct {
	Ref       string // node/123, way/456
	Name      string
	Tags      map[string]string
	Latitude  float64
	Longitude float64
	Address   Address // its own addr:* tags; HouseNumber "" when it has none
}

// Address is a street address.
type Address struct {
	Ref         string
	HouseNumber string
	Street      string
	Unit        string
	City        string
	State       string
	Postcode    string
	Latitude    float64
	Longitude   float64
}

// Line writes the address out: "12 Main Street, Town, ST 00000".
func (address Address) Line() string {
	if address.HouseNumber == "" || address.Street == "" {
		return ""
	}
	parts := []string{address.HouseNumber + " " + address.Street}
	if address.Unit != "" {
		parts[0] += " #" + strings.TrimLeft(address.Unit, "# ")
	}
	if address.City != "" {
		parts = append(parts, address.City)
	}
	if region := strings.TrimSpace(address.State + " " + address.Postcode); region != "" {
		parts = append(parts, region)
	}
	return strings.Join(parts, ", ")
}

// Extract is what was read from one or more files, with a grid to find
// what lies near a coordinate.
type Extract struct {
	Features  []Feature
	Addresses []Address
	// byStreetAddress finds addresses by their normalized house number and
	// street.
	byStreetAddress map[string][]int
	featureGrid     map[gridCell][]int
	addressGrid     map[gridCell][]int
}

// gridDegrees is the side of a grid cell: about 1.1 km of latitude. A
// search reaches at most one cell beyond its own, so radii stay under that.
const gridDegrees = 0.01

// MaximumSearchMeters is the largest radius Near answers for.
const MaximumSearchMeters = 800

type gridCell struct{ latitude, longitude int32 }

func cellOf(latitude, longitude float64) gridCell {
	return gridCell{int32(math.Floor(latitude / gridDegrees)), int32(math.Floor(longitude / gridDegrees))}
}

func isFeature(tags osm.Tags) bool {
	if strings.TrimSpace(tags.Find("name")) == "" {
		return false
	}
	for _, key := range typeKeys {
		if tags.Find(key) != "" {
			return true
		}
	}
	return false
}

func hasAddress(tags osm.Tags) bool {
	return tags.Find("addr:housenumber") != "" && tags.Find("addr:street") != ""
}

func wanted(tags osm.Tags) bool { return isFeature(tags) || hasAddress(tags) }

func addressOf(ref string, tags osm.Tags, latitude, longitude float64) Address {
	return Address{Ref: ref, HouseNumber: strings.TrimSpace(tags.Find("addr:housenumber")), Street: strings.TrimSpace(tags.Find("addr:street")),
		Unit: strings.TrimSpace(tags.Find("addr:unit")), City: strings.TrimSpace(tags.Find("addr:city")),
		State: strings.TrimSpace(tags.Find("addr:state")), Postcode: strings.TrimSpace(tags.Find("addr:postcode")),
		Latitude: latitude, Longitude: longitude}
}

// pendingWay is a way whose coordinate waits on its nodes.
type pendingWay struct {
	ref   string
	tags  osm.Tags
	nodes []osm.NodeID
}

// Read reads every file. Ways are placed at the mean of their nodes, which
// takes a second pass over each file for the nodes' coordinates.
// Relations are not read.
func Read(ctx context.Context, paths []string) (*Extract, error) {
	extract := &Extract{byStreetAddress: map[string][]int{}, featureGrid: map[gridCell][]int{}, addressGrid: map[gridCell][]int{}}
	for _, path := range paths {
		if err := extract.readFile(ctx, path); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return extract, nil
}

func scan(ctx context.Context, path string, configure func(*osmpbf.Scanner), each func(osm.Object)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := osmpbf.New(ctx, file, runtime.GOMAXPROCS(0))
	defer scanner.Close()
	configure(scanner)
	for scanner.Scan() {
		each(scanner.Object())
	}
	return scanner.Err()
}

func (extract *Extract) readFile(ctx context.Context, path string) error {
	var ways []pendingWay
	err := scan(ctx, path, func(scanner *osmpbf.Scanner) {
		scanner.SkipRelations = true
		scanner.FilterNode = func(node *osm.Node) bool { return wanted(node.Tags) }
		scanner.FilterWay = func(way *osm.Way) bool { return wanted(way.Tags) }
	}, func(object osm.Object) {
		switch typed := object.(type) {
		case *osm.Node:
			extract.add(fmt.Sprintf("node/%d", typed.ID), typed.Tags, typed.Lat, typed.Lon)
		case *osm.Way:
			nodes := make([]osm.NodeID, len(typed.Nodes))
			for position, node := range typed.Nodes {
				nodes[position] = node.ID
			}
			ways = append(ways, pendingWay{ref: fmt.Sprintf("way/%d", typed.ID), tags: typed.Tags, nodes: nodes})
		}
	})
	if err != nil || len(ways) == 0 {
		return err
	}
	var needed []osm.NodeID
	for _, way := range ways {
		needed = append(needed, way.nodes...)
	}
	sort.Slice(needed, func(a, b int) bool { return needed[a] < needed[b] })
	needed = compact(needed)
	latitudes, longitudes := make([]float64, len(needed)), make([]float64, len(needed))
	found := make([]bool, len(needed))
	err = scan(ctx, path, func(scanner *osmpbf.Scanner) {
		scanner.SkipWays, scanner.SkipRelations = true, true
		scanner.FilterNode = func(node *osm.Node) bool {
			position := sort.Search(len(needed), func(index int) bool { return needed[index] >= node.ID })
			return position < len(needed) && needed[position] == node.ID
		}
	}, func(object osm.Object) {
		node := object.(*osm.Node)
		position := sort.Search(len(needed), func(index int) bool { return needed[index] >= node.ID })
		latitudes[position], longitudes[position], found[position] = node.Lat, node.Lon, true
	})
	if err != nil {
		return err
	}
	for _, way := range ways {
		var latitude, longitude float64
		count := 0
		for _, id := range way.nodes {
			position := sort.Search(len(needed), func(index int) bool { return needed[index] >= id })
			if !found[position] {
				continue
			}
			latitude, longitude, count = latitude+latitudes[position], longitude+longitudes[position], count+1
		}
		if count == 0 {
			continue // its nodes lie outside the extract
		}
		extract.add(way.ref, way.tags, latitude/float64(count), longitude/float64(count))
	}
	return nil
}

func compact(sorted []osm.NodeID) []osm.NodeID {
	out := sorted[:0]
	for position, id := range sorted {
		if position == 0 || id != sorted[position-1] {
			out = append(out, id)
		}
	}
	return out
}

func (extract *Extract) add(ref string, tags osm.Tags, latitude, longitude float64) {
	cell := cellOf(latitude, longitude)
	var address Address
	if hasAddress(tags) {
		address = addressOf(ref, tags, latitude, longitude)
		extract.Addresses = append(extract.Addresses, address)
		position := len(extract.Addresses) - 1
		extract.addressGrid[cell] = append(extract.addressGrid[cell], position)
		key := streetAddressKey(address.HouseNumber, address.Street)
		extract.byStreetAddress[key] = append(extract.byStreetAddress[key], position)
	}
	if isFeature(tags) {
		kept := map[string]string{}
		for _, key := range keptKeys {
			if value := tags.Find(key); value != "" {
				kept[key] = value
			}
		}
		extract.Features = append(extract.Features, Feature{Ref: ref, Name: strings.TrimSpace(tags.Find("name")), Tags: kept,
			Latitude: latitude, Longitude: longitude, Address: address})
		extract.featureGrid[cell] = append(extract.featureGrid[cell], len(extract.Features)-1)
	}
}

func nearbyCells(latitude, longitude float64) []gridCell {
	center := cellOf(latitude, longitude)
	cells := make([]gridCell, 0, 9)
	for latitudeStep := int32(-1); latitudeStep <= 1; latitudeStep++ {
		for longitudeStep := int32(-1); longitudeStep <= 1; longitudeStep++ {
			cells = append(cells, gridCell{center.latitude + latitudeStep, center.longitude + longitudeStep})
		}
	}
	return cells
}

// FeatureNear is a feature and how far it lies from where was asked.
type FeatureNear struct {
	Feature
	DistanceMeters float64
}

// FeaturesNear is every feature within meters (at most
// MaximumSearchMeters) of the coordinate, nearest first.
func (extract *Extract) FeaturesNear(latitude, longitude, meters float64) []FeatureNear {
	meters = min(meters, MaximumSearchMeters)
	var near []FeatureNear
	for _, cell := range nearbyCells(latitude, longitude) {
		for _, position := range extract.featureGrid[cell] {
			feature := extract.Features[position]
			if distance := geonames.DistanceMeters(latitude, longitude, feature.Latitude, feature.Longitude); distance <= meters {
				near = append(near, FeatureNear{Feature: feature, DistanceMeters: distance})
			}
		}
	}
	sort.SliceStable(near, func(a, b int) bool { return near[a].DistanceMeters < near[b].DistanceMeters })
	return near
}

// NearestAddress is the nearest address within meters (at most
// MaximumSearchMeters), and its distance; ok false when there is none.
func (extract *Extract) NearestAddress(latitude, longitude, meters float64) (Address, float64, bool) {
	meters = min(meters, MaximumSearchMeters)
	var best Address
	bestDistance, ok := math.Inf(1), false
	for _, cell := range nearbyCells(latitude, longitude) {
		for _, position := range extract.addressGrid[cell] {
			address := extract.Addresses[position]
			if distance := geonames.DistanceMeters(latitude, longitude, address.Latitude, address.Longitude); distance <= meters && distance < bestDistance {
				best, bestDistance, ok = address, distance, true
			}
		}
	}
	return best, bestDistance, ok
}

func distance(latitudeA, longitudeA, latitudeB, longitudeB float64) float64 {
	return geonames.DistanceMeters(latitudeA, longitudeA, latitudeB, longitudeB)
}

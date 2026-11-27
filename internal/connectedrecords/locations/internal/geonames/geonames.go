// Package geonames names the town nearest a coordinate, offline, from the
// GeoNames dump of towns with at least 1000 people (cities1000), with the
// first-level region (admin1) and country names beside it. Download fetches
// the three public files once; Load reads them into memory, and Nearest
// answers from memory: no coordinate ever leaves the process.
package geonames

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The public files, as GeoNames publishes them. Download fetches each whole;
// the request carries nothing about whoever asks.
const (
	CitiesURL    = "https://download.geonames.org/export/dump/cities1000.zip"
	RegionsURL   = "https://download.geonames.org/export/dump/admin1CodesASCII.txt"
	CountriesURL = "https://download.geonames.org/export/dump/countryInfo.txt"

	CitiesFile    = "cities1000.txt"
	RegionsFile   = "admin1CodesASCII.txt"
	CountriesFile = "countryInfo.txt"
)

// Source names the data on every locality it gives, as who wrote it.
const Source = "geonames cities1000"

// City is one town of the dump.
type City struct {
	GeonamesID  int64
	Name        string
	Latitude    float64
	Longitude   float64
	CountryCode string
	RegionCode  string
	Population  int64
	TimeZone    string
}

// Match is the town Nearest chose for a coordinate.
type Match struct {
	City
	Region  string
	Country string
	// DistanceMeters is from the coordinate to the town's center.
	DistanceMeters float64
}

// Index holds every town in memory, bucketed by whole degree.
type Index struct {
	cells     map[[2]int][]City
	regions   map[string]string // "XX.01" → "Example Region"
	countries map[string]string // "XX" → "Exampleland"
	// byName holds every town under its name and its ASCII name, lowercased,
	// so a town named in text is found without asking anyone.
	byName map[string][]City
	Cities int
}

// Present says whether all three files are in dir.
func Present(dir string) bool {
	for _, name := range []string{CitiesFile, RegionsFile, CountriesFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// Download fetches the three files into dir, each written to a temporary
// name first so a broken download never leaves half a file in place.
func Download(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	fetch := func(url, path string) error {
		response, err := client.Get(url)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("fetch %s: %s", url, response.Status)
		}
		file, err := os.CreateTemp(dir, ".download-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := io.Copy(file, response.Body); err != nil {
			file.Close()
			return fmt.Errorf("fetch %s: %w", url, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), path)
	}
	zipPath := filepath.Join(dir, "cities1000.zip")
	if err := fetch(CitiesURL, zipPath); err != nil {
		return err
	}
	if err := unzipOne(zipPath, CitiesFile, filepath.Join(dir, CitiesFile)); err != nil {
		return err
	}
	if err := os.Remove(zipPath); err != nil {
		return err
	}
	if err := fetch(RegionsURL, filepath.Join(dir, RegionsFile)); err != nil {
		return err
	}
	return fetch(CountriesURL, filepath.Join(dir, CountriesFile))
}

func unzipOne(zipPath, name, destination string) error {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", zipPath, err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		file, err := os.CreateTemp(filepath.Dir(destination), ".unzip-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := io.Copy(file, reader); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), destination)
	}
	return fmt.Errorf("%s holds no %s", zipPath, name)
}

// Feature codes of places that are not a living town in their own right:
// a part of a bigger town, or one abandoned, destroyed or only historical.
var skippedFeatureCodes = map[string]bool{"PPLX": true, "PPLH": true, "PPLQ": true, "PPLW": true, "PPLCH": true}

// Load reads the three files from dir.
func Load(dir string) (*Index, error) {
	index := &Index{cells: map[[2]int][]City{}, regions: map[string]string{}, countries: map[string]string{}, byName: map[string][]City{}}
	err := eachLine(filepath.Join(dir, RegionsFile), func(fields []string) error {
		if len(fields) < 2 {
			return errors.New("a region line has fewer than 2 fields")
		}
		index.regions[fields[0]] = fields[1]
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = eachLine(filepath.Join(dir, CountriesFile), func(fields []string) error {
		if len(fields) < 5 {
			return errors.New("a country line has fewer than 5 fields")
		}
		index.countries[fields[0]] = fields[4]
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = eachLine(filepath.Join(dir, CitiesFile), func(fields []string) error {
		if len(fields) < 18 {
			return fmt.Errorf("a town line has %d fields, want 19", len(fields))
		}
		if skippedFeatureCodes[fields[7]] {
			return nil
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return fmt.Errorf("town id %q: %w", fields[0], err)
		}
		latitude, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			return fmt.Errorf("town %d latitude: %w", id, err)
		}
		longitude, err := strconv.ParseFloat(fields[5], 64)
		if err != nil {
			return fmt.Errorf("town %d longitude: %w", id, err)
		}
		population, _ := strconv.ParseInt(fields[14], 10, 64)
		city := City{GeonamesID: id, Name: fields[1], Latitude: latitude, Longitude: longitude, CountryCode: fields[8],
			RegionCode: fields[10], Population: population, TimeZone: fields[17]}
		cell := cellOf(latitude, longitude)
		index.cells[cell] = append(index.cells[cell], city)
		for _, name := range uniqueNames(fields[1], fields[2]) {
			index.byName[name] = append(index.byName[name], city)
		}
		index.Cities++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if index.Cities == 0 {
		return nil, fmt.Errorf("%s holds no towns", filepath.Join(dir, CitiesFile))
	}
	return index, nil
}

func eachLine(path string, read func(fields []string) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if err := read(strings.Split(text, "\t")); err != nil {
			return fmt.Errorf("%s line %d: %w", filepath.Base(path), line, err)
		}
	}
	return scanner.Err()
}

func cellOf(latitude, longitude float64) [2]int {
	return [2]int{int(math.Floor(latitude)), int(math.Floor(longitude))}
}

func uniqueNames(name, asciiName string) []string {
	first, second := strings.ToLower(strings.TrimSpace(name)), strings.ToLower(strings.TrimSpace(asciiName))
	if second == "" || second == first {
		return []string{first}
	}
	return []string{first, second}
}

// Named is every town whose name or ASCII name is name, in any case. It
// reads only the files in memory.
func (index *Index) Named(name string) []City {
	return index.byName[strings.ToLower(strings.TrimSpace(name))]
}

// RegionName is the name of a town's first-level region (Nevada for US.NV),
// or "" when the files hold none.
func (index *Index) RegionName(city City) string {
	return index.regions[city.CountryCode+"."+city.RegionCode]
}

// ReachMeters is how far from its center a town reaches, as Nearest counts
// it.
func ReachMeters(city City) float64 { return reachMeters(city.Population) }

// reachMeters is how far from its center a town of this many people reaches,
// roughly: 15 m per square root of a person, so about 12 km for 650,000
// people and 500 m for 1,000.
func reachMeters(population int64) float64 {
	return 15 * math.Sqrt(float64(population))
}

// Nearest is the town a coordinate belongs to: of the towns whose reach
// covers it, the one with the most people, so a point in a big town's suburb
// is named for the big town; when none reaches it, the nearest town. ok is
// false only when no town lies within about two degrees.
func (index *Index) Nearest(latitude, longitude float64) (Match, bool) {
	var nearest, biggestCovering *City
	nearestDistance, coveringDistance := math.Inf(1), 0.0
	home := cellOf(latitude, longitude)
	for ring := 0; ring <= 2; ring++ {
		for dLatitude := -ring; dLatitude <= ring; dLatitude++ {
			for dLongitude := -ring; dLongitude <= ring; dLongitude++ {
				if max(abs(dLatitude), abs(dLongitude)) != ring {
					continue
				}
				cell := [2]int{home[0] + dLatitude, wrapLongitudeCell(home[1] + dLongitude)}
				for position := range index.cells[cell] {
					city := &index.cells[cell][position]
					distance := DistanceMeters(latitude, longitude, city.Latitude, city.Longitude)
					if distance < nearestDistance {
						nearest, nearestDistance = city, distance
					}
					if distance <= reachMeters(city.Population) && (biggestCovering == nil || city.Population > biggestCovering.Population) {
						biggestCovering, coveringDistance = city, distance
					}
				}
			}
		}
		// One ring past the first find is enough: a degree is at least the
		// widest reach any town has.
		if nearest != nil && ring >= 1 {
			break
		}
	}
	chosen, distance := nearest, nearestDistance
	if biggestCovering != nil {
		chosen, distance = biggestCovering, coveringDistance
	}
	if chosen == nil {
		return Match{}, false
	}
	return Match{City: *chosen, Region: index.regions[chosen.CountryCode+"."+chosen.RegionCode],
		Country: index.countries[chosen.CountryCode], DistanceMeters: distance}, true
}

func wrapLongitudeCell(cell int) int {
	switch {
	case cell < -180:
		return cell + 360
	case cell >= 180:
		return cell - 360
	}
	return cell
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// DistanceMeters is the great-circle distance between two coordinates.
func DistanceMeters(latitudeA, longitudeA, latitudeB, longitudeB float64) float64 {
	const earthRadiusMeters = 6371000.0
	radians := math.Pi / 180
	dLatitude := (latitudeB - latitudeA) * radians
	dLongitude := (longitudeB - longitudeA) * radians
	a := math.Sin(dLatitude/2)*math.Sin(dLatitude/2) +
		math.Cos(latitudeA*radians)*math.Cos(latitudeB*radians)*math.Sin(dLongitude/2)*math.Sin(dLongitude/2)
	return 2 * earthRadiusMeters * math.Asin(math.Min(1, math.Sqrt(a)))
}

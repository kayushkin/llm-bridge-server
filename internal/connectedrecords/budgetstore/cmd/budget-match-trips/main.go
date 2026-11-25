// Command budget-match-trips reads location-store's journeys and asks
// budget-store to suggest, for each, the spending dated during it. It
// decides nothing: every link it makes is a suggestion the operator
// confirms or removes.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const commandName = "budget-match-trips"

func settingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: "budget_store_url", EnvironmentVariable: "BUDGET_TRIPS_BUDGET_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "http://127.0.0.1:8340",
			Description: "Where budget-store answers."},
		{Key: "location_store_url", EnvironmentVariable: "BUDGET_TRIPS_LOCATION_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Where location-store answers; its GET /journeys lists the trips."},
		{Key: "timezone", EnvironmentVariable: "BUDGET_TRIPS_TIMEZONE", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The operator's time zone (an IANA name); a journey's start and end times become the local days the bank dates charges by."},
		{Key: "home_markers", EnvironmentVariable: "BUDGET_TRIPS_HOME_MARKERS", Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "Comma-separated text that marks a charge as made at home (a city and state as the bank prints them); such charges are never suggested for a trip."},
	}
}

type journey struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at"`
	Title     string `json:"title"`
}

func main() {
	since := flag.String("since", "", "only journeys that ended on or after this day (YYYY-MM-DD); empty for all")
	flag.Parse()
	settings, err := servicesettings.New(commandName, []string{"BUDGET_TRIPS_"}, settingDefinitions(), servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	location, err := time.LoadLocation(settings.String("timezone"))
	if err != nil {
		log.Fatalf("timezone: %v", err)
	}
	markers := []string{}
	for _, m := range strings.Split(settings.String("home_markers"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			markers = append(markers, m)
		}
	}
	if len(markers) == 0 {
		log.Fatalf("BUDGET_TRIPS_HOME_MARKERS names no marker")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	var journeys []journey
	if err := call(client, http.MethodGet, strings.TrimRight(settings.String("location_store_url"), "/")+"/journeys?limit=5000", nil, &journeys); err != nil {
		log.Fatalf("read journeys: %v", err)
	}
	budget := strings.TrimRight(settings.String("budget_store_url"), "/")
	total := 0
	for _, j := range journeys {
		if j.Kind != "journey" || j.StartedAt == 0 || j.EndedAt == 0 {
			continue
		}
		first := time.Unix(j.StartedAt, 0).In(location).Format("2006-01-02")
		last := time.Unix(j.EndedAt, 0).In(location).Format("2006-01-02")
		if *since != "" && last < *since {
			continue
		}
		body, err := json.Marshal(map[string]any{"first_day": first, "last_day": last, "home_markers": markers})
		if err != nil {
			log.Fatal(err)
		}
		var answer struct {
			Suggested int `json:"suggested"`
		}
		if err := call(client, http.MethodPost, fmt.Sprintf("%s/trips/%s/suggest", budget, j.ID), body, &answer); err != nil {
			log.Fatalf("suggest for %s: %v", j.ID, err)
		}
		if answer.Suggested > 0 {
			log.Printf("%s (%s to %s, %s): %d suggested", j.ID, first, last, j.Title, answer.Suggested)
		}
		total += answer.Suggested
	}
	log.Printf("%d suggestions across %d journeys", total, len(journeys))
}

func call(client *http.Client, method, url string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %s: %s", method, url, response.Status, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, into)
}

// Command calendar-store-sync-google reads one Google calendar through dash
// and imports its events into calendar-store: each series once, as written,
// with its changed and deleted dates (dash's singleEvents=false and
// showDeleted=true). The calendar is a calendar-store calendar whose source is
// google; its source_ref is the Google calendar id dash is asked for.
//
// dash's token comes from DASH_TOKEN and is sent to dash alone, never printed.
// The import record is printed as JSON; any failure exits non-zero.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	calendarstore "github.com/kayushkin/calendar-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	calendarStoreURL := flag.String("calendar-store-url", "http://"+calendarstore.DefaultListenAddress, "where calendar-store answers")
	calendarID := flag.String("calendar", "", "the calendar-store calendar to fill (calendar_000001); its source must be google")
	from := flag.String("from", today.AddDate(-1, 0, 0).Format(time.DateOnly), "first day to read (YYYY-MM-DD, UTC)")
	to := flag.String("to", today.AddDate(2, 0, 0).Format(time.DateOnly), "day after the last day to read (YYYY-MM-DD, UTC)")
	flag.Parse()
	if *calendarID == "" {
		log.Fatal("-calendar is required")
	}
	settings, err := calendarstore.NewGoogleSyncSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	fromTime, err := time.Parse(time.DateOnly, *from)
	if err != nil {
		log.Fatalf("-from: %v", err)
	}
	toTime, err := time.Parse(time.DateOnly, *to)
	if err != nil {
		log.Fatalf("-to: %v", err)
	}
	if !toTime.After(fromTime) {
		log.Fatalf("-to %s is not after -from %s", *to, *from)
	}
	store := strings.TrimRight(*calendarStoreURL, "/")
	client := &http.Client{Timeout: 5 * time.Minute}

	var calendars []calendarstore.Calendar
	if err := getJSON(client, store+"/calendars", "", &calendars); err != nil {
		log.Fatal(err)
	}
	var calendar *calendarstore.Calendar
	for index := range calendars {
		if calendars[index].ID == *calendarID {
			calendar = &calendars[index]
		}
	}
	if calendar == nil {
		log.Fatalf("calendar-store holds no calendar %s", *calendarID)
	}
	if calendar.Source != "google" {
		log.Fatalf("calendar %s is kept by %s, not google", calendar.ID, calendar.Source)
	}

	query := url.Values{"calendarId": {calendar.SourceRef}, "timeMin": {fromTime.Format(time.RFC3339)},
		"timeMax": {toTime.Format(time.RFC3339)}, "singleEvents": {"false"}, "showDeleted": {"true"}}
	var listed struct {
		Events []json.RawMessage `json:"events"`
		Error  string            `json:"error"`
	}
	dash := strings.TrimRight(settings.String(calendarstore.SettingDashURL), "/")
	if err := getJSON(client, dash+"/api/calendar/google?"+query.Encode(), settings.String(calendarstore.SettingDashToken), &listed); err != nil {
		log.Fatal(err)
	}
	if listed.Error != "" {
		log.Fatalf("dash could not read calendar %s: %s", calendar.SourceRef, listed.Error)
	}
	if listed.Events == nil {
		log.Fatalf("dash's answer for calendar %s holds no events list", calendar.SourceRef)
	}
	body, err := json.Marshal(listed.Events)
	if err != nil {
		log.Fatal(err)
	}
	fileName := fmt.Sprintf("dash /api/calendar/google %s %s..%s", calendar.SourceRef, *from, *to)
	importURL := store + "/calendars/" + url.PathEscape(calendar.ID) + "/google-event-imports?" + url.Values{"file_name": {fileName}}.Encode()
	response, err := client.Post(importURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("POST import: %v", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		log.Fatalf("calendar-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	os.Stdout.Write(answer)
}

func getJSON(client *http.Client, address, bearerToken string, into any) error {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	if bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", request.URL.Path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d: %s", request.URL.Path, response.StatusCode, strings.TrimSpace(string(content)))
	}
	if err := json.Unmarshal(content, into); err != nil {
		return fmt.Errorf("GET %s: %w", request.URL.Path, err)
	}
	return nil
}

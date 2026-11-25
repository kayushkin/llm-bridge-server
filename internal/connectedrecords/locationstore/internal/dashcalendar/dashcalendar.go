// Package dashcalendar reads Google Calendar through dash, the one way to it
// on a host: the calendars dash lists and their events in a window. It sends
// dash's bearer token to dash alone and never prints it.
package dashcalendar

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	locationstore "github.com/kayushkin/location-store"
)

// Client reads dash's calendar routes.
type Client struct {
	url   string
	token string
	http  *http.Client
}

// New makes a client of the dash at address, with its bearer token.
func New(address, token string) *Client {
	return &Client{url: strings.TrimRight(address, "/"), token: token, http: &http.Client{Timeout: 5 * time.Minute}}
}

// Calendars lists every calendar dash lists.
func (c *Client) Calendars() ([]locationstore.GoogleCalendar, error) {
	var listed struct {
		Items []locationstore.GoogleCalendar `json:"items"`
	}
	if err := c.get("/api/calendar/google/calendars", nil, &listed); err != nil {
		return nil, err
	}
	return listed.Items, nil
}

// Events reads one calendar's events from from to to.
func (c *Client) Events(calendar locationstore.GoogleCalendar, from, to time.Time) ([]locationstore.GoogleCalendarEvent, error) {
	var events struct {
		Events []locationstore.GoogleCalendarEvent `json:"events"`
	}
	query := url.Values{"calendarId": {calendar.ID}, "timeMin": {from.Format(time.RFC3339)}, "timeMax": {to.Format(time.RFC3339)},
		"maxResults": {"2500"}}
	if err := c.get("/api/calendar/google", query, &events); err != nil {
		return nil, fmt.Errorf("calendar %s: %w", calendar.ID, err)
	}
	return events.Events, nil
}

func (c *Client) get(path string, query url.Values, answer any) error {
	address := c.url + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: dash answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(content)))
	}
	if err := json.Unmarshal(content, answer); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

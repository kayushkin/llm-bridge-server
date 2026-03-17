package calendar

import "time"

// Event represents a calendar event.
type Event struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Start       time.Time  `json:"start"`
	End         time.Time  `json:"end"`
	AllDay      bool       `json:"all_day,omitempty"`
	Location    string     `json:"location,omitempty"`
	CalendarID  string     `json:"calendar_id,omitempty"`
	Recurring   bool       `json:"recurring,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ListEventsRequest is the request for listing calendar events.
type ListEventsRequest struct {
	CalendarID string    `json:"calendar_id,omitempty"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	MaxResults int       `json:"max_results,omitempty"`
}

// ListEventsResponse is the response for listing calendar events.
type ListEventsResponse struct {
	Events []Event `json:"events"`
	Total  int     `json:"total"`
}

// CreateEventRequest is the request for creating a calendar event.
type CreateEventRequest struct {
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day,omitempty"`
	Location    string    `json:"location,omitempty"`
	CalendarID  string    `json:"calendar_id,omitempty"`
}

// CreateEventResponse is the response for creating a calendar event.
type CreateEventResponse struct {
	Event Event `json:"event"`
}

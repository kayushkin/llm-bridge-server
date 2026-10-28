package activitystore

import (
	"fmt"
	"sort"
	"time"
)

// MaxDayFirst bounds how many activities a day summary lists per product.
const MaxDayFirst = 50

// Day summarises one calendar day in a time zone: for each product, how many
// live activities, of which actions, and the earliest few. A day is midnight
// to midnight in the zone, so one with a clock change is 23 or 25 hours.
func (s *Store) Day(date, timeZone string, first int) (Day, error) {
	if timeZone == "" {
		return Day{}, fmt.Errorf("%w: tz is required, an IANA zone such as Europe/Paris: a day starts at midnight somewhere", ErrInvalid)
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return Day{}, fmt.Errorf("%w: tz %q is not an IANA time zone: %v", ErrInvalid, timeZone, err)
	}
	day, err := time.ParseInLocation("2006-01-02", date, location)
	if err != nil {
		return Day{}, fmt.Errorf("%w: date %q is not YYYY-MM-DD", ErrInvalid, date)
	}
	if first < 0 || first > MaxDayFirst {
		return Day{}, fmt.Errorf("%w: first must be 0 to %d", ErrInvalid, MaxDayFirst)
	}
	answer := Day{Date: date, TimeZone: timeZone, From: day.Unix(), To: day.AddDate(0, 0, 1).Unix(), Products: []DayProduct{}}
	rows, err := s.db.Query(`SELECT id, product, action, title, url, occurred_at FROM activities
		WHERE deleted_at = 0 AND occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at, seq`, answer.From, answer.To)
	if err != nil {
		return Day{}, err
	}
	defer rows.Close()
	byProduct := map[string]*DayProduct{}
	actionCounts := map[string]map[string]int{}
	for rows.Next() {
		var item DayItem
		var product string
		if err := rows.Scan(&item.ID, &product, &item.Action, &item.Title, &item.URL, &item.OccurredAt); err != nil {
			return Day{}, err
		}
		summary, found := byProduct[product]
		if !found {
			summary = &DayProduct{Product: product, FirstAt: item.OccurredAt, First: []DayItem{}}
			byProduct[product] = summary
			actionCounts[product] = map[string]int{}
		}
		summary.Count++
		summary.LastAt = item.OccurredAt
		actionCounts[product][item.Action]++
		if len(summary.First) < first {
			summary.First = append(summary.First, item)
		}
		answer.Total++
	}
	if err := rows.Err(); err != nil {
		return Day{}, err
	}
	for product, summary := range byProduct {
		summary.Actions = []NamedCount{}
		for action, count := range actionCounts[product] {
			summary.Actions = append(summary.Actions, NamedCount{Name: action, Count: count})
		}
		sort.Slice(summary.Actions, func(i, j int) bool {
			if summary.Actions[i].Count != summary.Actions[j].Count {
				return summary.Actions[i].Count > summary.Actions[j].Count
			}
			return summary.Actions[i].Name < summary.Actions[j].Name
		})
		answer.Products = append(answer.Products, *summary)
	}
	sort.Slice(answer.Products, func(i, j int) bool {
		if answer.Products[i].Count != answer.Products[j].Count {
			return answer.Products[i].Count > answer.Products[j].Count
		}
		return answer.Products[i].Product < answer.Products[j].Product
	})
	return answer, nil
}

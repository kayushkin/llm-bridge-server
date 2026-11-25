package photostore

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maximumTakenActivitySamples = 24

// TakenActivity answers GET /taken-activity: the photos the filter keeps,
// counted per span of time in the zone given, earliest first, with up to
// `samples` ids from each span spread evenly through it. Photos with no
// taken_at are left out. Paging in the filter is ignored.
func (s *Store) TakenActivity(filter Filter, interval string, location *time.Location, samples int) ([]TakenActivityBucket, error) {
	if !slices.Contains(TakenActivityIntervals, interval) {
		return nil, fmt.Errorf("%w: interval %q is not one of %v", ErrInvalid, interval, TakenActivityIntervals)
	}
	if samples < 0 || samples > maximumTakenActivitySamples {
		return nil, fmt.Errorf("%w: samples must be 0 to %d, got %d", ErrInvalid, maximumTakenActivitySamples, samples)
	}
	whereSQL, args, err := filter.whereClause()
	if err != nil {
		return nil, err
	}
	if whereSQL == "" {
		whereSQL = " WHERE taken_at > 0"
	} else {
		whereSQL += " AND taken_at > 0"
	}
	rows, err := s.db.Query(`SELECT id, taken_at FROM photos`+whereSQL+` ORDER BY taken_at, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := []TakenActivityBucket{}
	idsInBucket := [][]string{}
	for rows.Next() {
		var id string
		var takenAt int64
		if err := rows.Scan(&id, &takenAt); err != nil {
			return nil, err
		}
		start := takenSpanStart(time.Unix(takenAt, 0).In(location), interval)
		if len(buckets) == 0 || buckets[len(buckets)-1].Start != start {
			buckets = append(buckets, TakenActivityBucket{Start: start, SamplePhotoIDs: []string{}})
			idsInBucket = append(idsInBucket, nil)
		}
		buckets[len(buckets)-1].Photos++
		idsInBucket[len(idsInBucket)-1] = append(idsInBucket[len(idsInBucket)-1], id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range buckets {
		buckets[index].SamplePhotoIDs = evenlySpaced(idsInBucket[index], samples)
	}
	return buckets, nil
}

// takenSpanStart is the first instant of the span holding at, in at's zone.
func takenSpanStart(at time.Time, interval string) int64 {
	year, month, day := at.Date()
	location := at.Location()
	switch interval {
	case "day":
		return time.Date(year, month, day, 0, 0, 0, 0, location).Unix()
	case "week":
		offset := (int(at.Weekday()) + 6) % 7
		return time.Date(year, month, day-offset, 0, 0, 0, 0, location).Unix()
	case "month":
		return time.Date(year, month, 1, 0, 0, 0, 0, location).Unix()
	default:
		return time.Date(year, 1, 1, 0, 0, 0, 0, location).Unix()
	}
}

// evenlySpaced picks count items spread through ids, first and last
// included when count allows, in order.
func evenlySpaced(ids []string, count int) []string {
	if count <= 0 {
		return []string{}
	}
	if len(ids) <= count {
		return slices.Clone(ids)
	}
	picked := make([]string, 0, count)
	if count == 1 {
		return append(picked, ids[len(ids)/2])
	}
	for index := 0; index < count; index++ {
		picked = append(picked, ids[index*(len(ids)-1)/(count-1)])
	}
	return picked
}

func (h *handler) takenActivity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	timeZone := query.Get("time_zone")
	location, err := time.LoadLocation(timeZone)
	if err != nil || timeZone == "" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("time_zone %q is required and must be an IANA zone", timeZone))
		return
	}
	filter := Filter{Folder: query.Get("folder"), Query: query.Get("q"), Tag: query.Get("tag"), Person: query.Get("person"), Described: query.Get("described"),
		HasGPS: query.Get("has_gps") == "true", ExcludeTags: query["exclude_tag"]}
	var ok bool
	if filter.From, ok = integerParameter(w, r, "from"); !ok {
		return
	}
	if filter.To, ok = integerParameter(w, r, "to"); !ok {
		return
	}
	samples := 0
	if raw := strings.TrimSpace(query.Get("samples")); raw != "" {
		if samples, err = strconv.Atoi(raw); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("samples must be a whole number, got %q", raw))
			return
		}
	}
	buckets, err := h.s.TakenActivity(filter, query.Get("interval"), location, samples)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, buckets)
}

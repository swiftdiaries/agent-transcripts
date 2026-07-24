package analytics

import (
	"fmt"
	"net/url"
	"time"
)

type Range struct {
	Key, Label string
	Start, End time.Time
	All        bool
}

func ParseRange(values url.Values, now time.Time) (Range, error) {
	key := values.Get("range")
	if key == "" {
		key = "7d"
	}
	day := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if key == "all" {
		return Range{Key: key, Label: "All time", All: true}, nil
	}
	if key == "custom" {
		from, to := values.Get("from"), values.Get("to")
		if from == "" || to == "" {
			return Range{}, fmt.Errorf("custom range requires from and to dates")
		}
		start, e := time.Parse("2006-01-02", from)
		if e != nil {
			return Range{}, fmt.Errorf("invalid from date")
		}
		end, e := time.Parse("2006-01-02", to)
		if e != nil {
			return Range{}, fmt.Errorf("invalid to date")
		}
		if end.Before(start) {
			return Range{}, fmt.Errorf("from date must not follow to date")
		}
		return Range{Key: key, Label: "Custom", Start: start, End: end.AddDate(0, 0, 1)}, nil
	}
	days := map[string]int{"7d": 7, "30d": 30, "90d": 90}[key]
	if days == 0 {
		return Range{}, fmt.Errorf("invalid range")
	}
	return Range{Key: key, Label: map[string]string{"7d": "7 days", "30d": "30 days", "90d": "90 days"}[key], Start: day.AddDate(0, 0, -(days - 1)), End: day.AddDate(0, 0, 1)}, nil
}
func (r Range) Contains(value time.Time) bool {
	return r.All || (!value.Before(r.Start) && value.Before(r.End))
}

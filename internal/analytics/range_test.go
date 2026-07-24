package analytics

import (
	"net/url"
	"testing"
	"time"
)

func TestParseRangeDefaultsToSevenUTCCalendarDays(t *testing.T) {
	now := time.Date(2026, 7, 23, 14, 30, 0, 0, time.FixedZone("IST", 19800))
	got, err := ParseRange(url.Values{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) || got.End != time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("range=%#v", got)
	}
}
func TestParseRangeAcceptsInclusiveCustomDates(t *testing.T) {
	got, err := ParseRange(url.Values{"range": {"custom"}, "from": {"2026-07-01"}, "to": {"2026-07-03"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Contains(time.Date(2026, 7, 3, 23, 59, 0, 0, time.UTC)) || got.Contains(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("range=%#v", got)
	}
}

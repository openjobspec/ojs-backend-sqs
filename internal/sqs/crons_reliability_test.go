package sqs

import (
	"testing"
	"time"
)

func TestParseCronSchedule_AppliesStoredTimezoneEveryTime(t *testing.T) {
	schedule, err := parseCronSchedule("0 9 * * *", "America/New_York")
	if err != nil {
		t.Fatalf("parseCronSchedule: %v", err)
	}
	base := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	next := schedule.Next(base)
	if got, want := next.UTC(), time.Date(2026, time.July, 1, 13, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next New York run = %s, want %s", got, want)
	}

	// Advancing the returned schedule retains the same timezone across runs.
	second := schedule.Next(next)
	if got, want := second.UTC(), time.Date(2026, time.July, 2, 13, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("second New York run = %s, want %s", got, want)
	}
}

func TestParseCronSchedule_DefaultsToUTC(t *testing.T) {
	schedule, err := parseCronSchedule("0 9 * * *", "")
	if err != nil {
		t.Fatalf("parseCronSchedule: %v", err)
	}
	base := time.Date(2026, time.July, 1, 8, 30, 0, 0, time.UTC)
	if got, want := schedule.Next(base), time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next UTC run = %s, want %s", got, want)
	}
}

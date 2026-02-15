package core

import (
	"testing"
	"time"
)

func TestParseISO8601Duration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"PT1S", 1 * time.Second, false},
		{"PT30S", 30 * time.Second, false},
		{"PT1M", 1 * time.Minute, false},
		{"PT5M", 5 * time.Minute, false},
		{"PT1H", 1 * time.Hour, false},
		{"PT1H30M", 1*time.Hour + 30*time.Minute, false},
		{"PT1H30M45S", 1*time.Hour + 30*time.Minute + 45*time.Second, false},
		{"PT2H", 2 * time.Hour, false},
		{"PT0.5S", 500 * time.Millisecond, false},

		// Invalid
		{"", 0, true},
		{"P1D", 0, true},      // days not supported
		{"1S", 0, true},       // missing PT prefix
		{"PT", 0, true},       // zero duration
		{"invalid", 0, true},  // garbage
		{"PT0S", 0, true},     // zero duration
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseISO8601Duration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseISO8601Duration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseISO8601Duration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatISO8601Duration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "PT0S"},
		{1 * time.Second, "PT1S"},
		{30 * time.Second, "PT30S"},
		{1 * time.Minute, "PT1M"},
		{5 * time.Minute, "PT5M"},
		{1 * time.Hour, "PT1H"},
		{1*time.Hour + 30*time.Minute, "PT1H30M"},
		{1*time.Hour + 30*time.Minute + 45*time.Second, "PT1H30M45S"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatISO8601Duration(tt.input)
			if got != tt.want {
				t.Errorf("FormatISO8601Duration(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	durations := []string{"PT1S", "PT5M", "PT1H", "PT1H30M", "PT2H45M30S"}
	for _, s := range durations {
		d, err := ParseISO8601Duration(s)
		if err != nil {
			t.Fatalf("ParseISO8601Duration(%q) failed: %v", s, err)
		}
		got := FormatISO8601Duration(d)
		if got != s {
			t.Errorf("Round trip failed: %q -> %v -> %q", s, d, got)
		}
	}
}

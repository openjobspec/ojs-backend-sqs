package api

import "testing"

func TestFormatEventIDPreservesFullCounter(t *testing.T) {
	tests := []struct {
		id   uint64
		want string
	}{
		{id: 1, want: "evt_1"},
		{id: 10_001, want: "evt_10001"},
		{id: 1_000_000, want: "evt_1000000"},
	}

	for _, test := range tests {
		if got := formatEventID(test.id); got != test.want {
			t.Errorf("formatEventID(%d) = %q, want %q", test.id, got, test.want)
		}
	}
}

package retention

import (
	"testing"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

func TestNextRun(t *testing.T) {
	wib := httpx.WIB
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"before the run time, same day", time.Date(2026, 10, 22, 1, 0, 0, 0, wib), time.Date(2026, 10, 22, 2, 0, 0, 0, wib)},
		{"exactly at the run time, next day", time.Date(2026, 10, 22, 2, 0, 0, 0, wib), time.Date(2026, 10, 23, 2, 0, 0, 0, wib)},
		{"after the run time", time.Date(2026, 10, 22, 14, 30, 0, 0, wib), time.Date(2026, 10, 23, 2, 0, 0, 0, wib)},
		{"just before midnight", time.Date(2026, 10, 22, 23, 59, 59, 0, wib), time.Date(2026, 10, 23, 2, 0, 0, 0, wib)},
		{"month end", time.Date(2026, 10, 31, 3, 0, 0, 0, wib), time.Date(2026, 11, 1, 2, 0, 0, 0, wib)},
		// 19:30 UTC on the 22nd is 02:30 WIB on the 23rd: the next 02:00 WIB is the 24th.
		{"given in UTC", time.Date(2026, 10, 22, 19, 30, 0, 0, time.UTC), time.Date(2026, 10, 24, 2, 0, 0, 0, wib)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NextRun(tt.now, 2, 0); !got.Equal(tt.want) {
				t.Fatalf("NextRun(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
	if got, want := NextRun(time.Date(2026, 10, 22, 3, 0, 0, 0, httpx.WIB), 23, 45), time.Date(2026, 10, 22, 23, 45, 0, 0, httpx.WIB); !got.Equal(want) {
		t.Fatalf("with minutes: got %v, want %v", got, want)
	}
}

func TestParseRunAt(t *testing.T) {
	for in, want := range map[string][2]int{"02:00": {2, 0}, "23:45": {23, 45}, " 0:05 ": {0, 5}} {
		h, m, err := ParseRunAt(in)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("ParseRunAt(%q) = %d,%d,%v", in, h, m, err)
		}
	}
	for _, bad := range []string{"", "2", "24:00", "02:60", "ab:cd", "-1:00", "02-00"} {
		if _, _, err := ParseRunAt(bad); err == nil {
			t.Errorf("ParseRunAt(%q) should fail", bad)
		}
	}
}

func TestSettingsValidate(t *testing.T) {
	ok := []Settings{{30, 90}, {1, 1}, {90, 90}, {365, 1095}}
	bad := []Settings{{0, 90}, {30, 0}, {366, 1095}, {30, 1096}, {91, 90}, {-1, 90}}
	for _, s := range ok {
		if p := s.Validate(); len(p) != 0 {
			t.Errorf("%+v should be valid: %v", s, p)
		}
	}
	for _, s := range bad {
		if p := s.Validate(); len(p) == 0 {
			t.Errorf("%+v should be invalid", s)
		}
	}
}

package httpx

import "time"

// WIB is Western Indonesia Time. It has no DST, so a fixed zone avoids needing tzdata.
var WIB = time.FixedZone("WIB", 7*60*60)

// ParseDay reads a YYYY-MM-DD query value as midnight WIB. An empty value is not an
// error (ok=false, nothing recorded); a malformed one is recorded in fields under name.
func ParseDay(s, name string, fields map[string]string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	d, err := time.ParseInLocation("2006-01-02", s, WIB)
	if err != nil {
		fields[name] = "Format tanggal harus YYYY-MM-DD."
		return time.Time{}, false
	}
	return d, true
}

package cfg

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ParseHHMM parses a "HH:MM" clock time.
func ParseHHMM(s string) (hour, min int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hour, err = strconv.Atoi(hs)
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q has an hour outside 00-23", s)
	}
	min, err = strconv.Atoi(ms)
	if err != nil || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("%q has a minute outside 00-59", s)
	}
	return hour, min, nil
}

var weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// WeekdayIndex maps a weekday name to time.Weekday, or -1 if unrecognised.
func WeekdayIndex(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	for i, w := range weekdays {
		if w == n || (len(n) == 3 && strings.HasPrefix(w, n)) {
			return i
		}
	}
	return -1
}

// Location resolves the schedule's timezone, falling back to local time.
func (s Schedule) Location() *time.Location {
	if s.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// NextRun returns the first scheduled moment strictly after `after`.
// It returns the zero time when the schedule never fires on its own.
func (s Schedule) NextRun(after time.Time) time.Time {
	if s.Mode == "" || s.Mode == "manual" || len(s.Times) == 0 {
		return time.Time{}
	}
	loc := s.Location()
	now := after.In(loc)

	// Look ahead far enough to cross a month boundary.
	for day := 0; day <= 400; day++ {
		d := now.AddDate(0, 0, day)

		switch s.Mode {
		case "daily":
			// every day qualifies
		case "weekly":
			if int(d.Weekday()) != WeekdayIndex(s.DayOfWeek) {
				continue
			}
		case "monthly":
			if d.Day() != s.DayOfMonth {
				continue
			}
		default:
			return time.Time{}
		}

		best := time.Time{}
		for _, t := range s.Times {
			h, m, err := ParseHHMM(t)
			if err != nil {
				continue
			}
			cand := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
			if !cand.After(now) {
				continue
			}
			if best.IsZero() || cand.Before(best) {
				best = cand
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return time.Time{}
}

// Describe renders the schedule as a sentence for the console.
func (s Schedule) Describe() string {
	if s.Mode == "" || s.Mode == "manual" {
		return "manual (no automatic runs)"
	}
	times := strings.Join(s.Times, ", ")
	zone := s.Timezone
	if zone == "" {
		zone = "local time"
	}
	switch s.Mode {
	case "daily":
		return fmt.Sprintf("every day at %s (%s)", times, zone)
	case "weekly":
		return fmt.Sprintf("every %s at %s (%s)", strings.Title(s.DayOfWeek), times, zone) //nolint:staticcheck
	case "monthly":
		return fmt.Sprintf("on day %d of each month at %s (%s)", s.DayOfMonth, times, zone)
	}
	return s.Mode
}

// warnIfWorldReadable refuses to load a secrets file that other users can read.
// Windows permission bits are not meaningful here, so the check is skipped there.
func warnIfWorldReadable(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o); run: chmod 600 %s",
			path, fi.Mode().Perm(), path)
	}
	return nil
}

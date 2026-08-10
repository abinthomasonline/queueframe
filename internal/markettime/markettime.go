package markettime

import (
	"fmt"
	"time"
)

func Target(date, clock string, location *time.Location) (time.Time, error) {
	parsedDate, err := time.ParseInLocation("2006-01-02", date, location)
	if err != nil {
		return time.Time{}, fmt.Errorf("date: %w", err)
	}
	parsedClock, err := time.Parse("15:04:05", clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock: %w", err)
	}
	return time.Date(parsedDate.Year(), parsedDate.Month(), parsedDate.Day(), parsedClock.Hour(), parsedClock.Minute(), parsedClock.Second(), 0, location), nil
}

func NextWeekday(date time.Time) time.Time {
	next := date.AddDate(0, 0, 1)
	for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

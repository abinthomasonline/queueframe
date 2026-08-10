package markettime

import (
	"testing"
	"time"
)

func TestTargetUsesKolkata(t *testing.T) {
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Target("2026-08-07", "09:00:00", location)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hour() != 9 || got.Minute() != 0 || got.Location() != location {
		t.Fatalf("unexpected target %v", got)
	}
}

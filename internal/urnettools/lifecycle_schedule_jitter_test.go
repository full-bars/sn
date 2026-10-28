package urnettools

import (
	"strconv"
	"strings"
	"testing"
)

func TestBSDCronWeeklyMinuteJitter(t *testing.T) {
	for _, minute := range []int{0, 17, 42, 59} {
		got, err := bsdCronSchedule("weekly", minute)
		if err != nil {
			t.Fatalf("bsdCronSchedule(weekly, %d): %v", minute, err)
		}
		want := strings.Join([]string{strconv.Itoa(minute), "3 * * 0"}, " ")
		if got != want {
			t.Errorf("weekly schedule at minute %d = %q, want %q", minute, got, want)
		}
	}
	if _, err := bsdCronSchedule("weekly", 60); err == nil {
		t.Fatal("weekly schedule accepted an out-of-range minute")
	}
	if got, err := bsdCronSchedule("daily", 42); err != nil || got != "17 3 * * *" {
		t.Errorf("daily schedule = %q, %v; want fixed 03:17", got, err)
	}
	if got, err := bsdCronSchedule("monthly", 42); err != nil || got != "17 3 1 * *" {
		t.Errorf("monthly schedule = %q, %v; want fixed 03:17 on day one", got, err)
	}
	for range 32 {
		minute, err := randomBSDCronMinute()
		if err != nil {
			t.Fatal(err)
		}
		if minute < 0 || minute > 59 {
			t.Errorf("randomBSDCronMinute() = %d, outside 0-59", minute)
		}
	}
}

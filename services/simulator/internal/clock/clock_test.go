package clock

import (
	"testing"
	"time"
)

func TestSinceMidnightUsesTokyoTime(t *testing.T) {
	// 02:30 UTC is 11:30 in Tokyo.
	utc := time.Date(2026, 10, 14, 2, 30, 15, 0, time.UTC)
	if got, want := SinceMidnight(utc), 11*time.Hour+30*time.Minute+15*time.Second; got != want {
		t.Errorf("SinceMidnight = %v, want %v", got, want)
	}
	// 20:00 UTC is 05:00 the next day in Tokyo.
	late := time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)
	if got := SinceMidnight(late); got != 5*time.Hour {
		t.Errorf("SinceMidnight = %v, want 5h", got)
	}
}

func TestClockRunsAtRealSpeedFromTheChosenStart(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	c := New(start, 11*time.Hour, 10)

	if got := c.TimeOfDay(start); got != 11*time.Hour {
		t.Errorf("at start: %v, want 11h", got)
	}
	// Ninety real minutes later it is 12:30 in the simulation, whatever the speed-up.
	if got := c.TimeOfDay(start.Add(90 * time.Minute)); got != 12*time.Hour+30*time.Minute {
		t.Errorf("after 90 min: %v, want 12h30m", got)
	}
	// The day wraps at midnight, but SimTime keeps counting.
	later := start.Add(14 * time.Hour)
	if got := c.TimeOfDay(later); got != time.Hour {
		t.Errorf("TimeOfDay after wrap = %v, want 1h", got)
	}
	if got := c.SimTime(later); got != 25*time.Hour {
		t.Errorf("SimTime after wrap = %v, want 25h", got)
	}
	if got := c.RealTime(25 * time.Hour); !got.Equal(later) {
		t.Errorf("RealTime = %v, want %v", got, later)
	}
}

func TestScaleAndSpeedup(t *testing.T) {
	c := New(time.Now(), 0, 10)
	if c.Speedup() != 10 {
		t.Errorf("Speedup = %v", c.Speedup())
	}
	if got := c.Scale(10 * time.Minute); got != time.Minute {
		t.Errorf("Scale(10m) = %v, want 1m", got)
	}
	if slow := New(time.Now(), 0, 0); slow.Speedup() != 1 {
		t.Errorf("speed-up below 1 should become 1, got %v", slow.Speedup())
	}
}

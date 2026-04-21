package cronjobs

import (
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/tools"
	"github.com/ParthSareen/OllamaClaw/internal/util"
)

func TestCompileReminderSpecPacificOnce(t *testing.T) {
	now := time.Date(2026, time.April, 20, 9, 0, 0, 0, util.PacificLocation())
	out, err := CompileReminderSpecPacific(tools.ReminderSpec{
		Mode: "once",
		Date: "2026-04-20",
		Time: "09:05",
	}, now)
	if err != nil {
		t.Fatalf("CompileReminderSpecPacific() error: %v", err)
	}
	if out.Mode != "once" {
		t.Fatalf("expected mode once, got %q", out.Mode)
	}
	if out.CompiledSchedule != "5 9 20 4 *" {
		t.Fatalf("unexpected schedule: %q", out.CompiledSchedule)
	}
	if out.OnceFireAt == nil {
		t.Fatalf("expected OnceFireAt to be set")
	}
	if !strings.Contains(out.NormalizedSpecJSON, `"mode":"once"`) {
		t.Fatalf("expected normalized JSON to include once mode, got %q", out.NormalizedSpecJSON)
	}
}

func TestCompileReminderSpecPacificOnceRejectsPast(t *testing.T) {
	now := time.Date(2026, time.April, 20, 9, 0, 0, 0, util.PacificLocation())
	_, err := CompileReminderSpecPacific(tools.ReminderSpec{
		Mode: "once",
		Date: "2026-04-20",
		Time: "08:59",
	}, now)
	if err == nil {
		t.Fatalf("expected past once reminder to fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "future") {
		t.Fatalf("expected future validation error, got %v", err)
	}
}

func TestCompileReminderSpecPacificIntervalModes(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		spec tools.ReminderSpec
		want string
	}{
		{
			name: "minute",
			spec: tools.ReminderSpec{Mode: "interval", IntervalUnit: "minute", Interval: 15},
			want: "*/15 * * * *",
		},
		{
			name: "hour",
			spec: func() tools.ReminderSpec {
				m := 7
				return tools.ReminderSpec{Mode: "interval", IntervalUnit: "hour", Interval: 2, Minute: &m}
			}(),
			want: "7 */2 * * *",
		},
		{
			name: "day",
			spec: tools.ReminderSpec{Mode: "interval", IntervalUnit: "day", Interval: 3, Time: "14:30"},
			want: "30 14 */3 * *",
		},
	}
	for _, tc := range tests {
		out, err := CompileReminderSpecPacific(tc.spec, now)
		if err != nil {
			t.Fatalf("%s compile error: %v", tc.name, err)
		}
		if out.CompiledSchedule != tc.want {
			t.Fatalf("%s expected schedule %q, got %q", tc.name, tc.want, out.CompiledSchedule)
		}
	}
}

func TestCompileReminderSpecPacificWeekdaysAndMonthly(t *testing.T) {
	now := time.Now()
	weekdays, err := CompileReminderSpecPacific(tools.ReminderSpec{
		Mode: "weekdays",
		Days: []string{"fri", "mon", "wed", "mon"},
		Time: "09:00",
	}, now)
	if err != nil {
		t.Fatalf("weekdays compile error: %v", err)
	}
	if weekdays.CompiledSchedule != "0 9 * * 1,3,5" {
		t.Fatalf("unexpected weekdays schedule %q", weekdays.CompiledSchedule)
	}

	monthly, err := CompileReminderSpecPacific(tools.ReminderSpec{
		Mode:       "monthly",
		DayOfMonth: 21,
		Time:       "18:45",
	}, now)
	if err != nil {
		t.Fatalf("monthly compile error: %v", err)
	}
	if monthly.CompiledSchedule != "45 18 21 * *" {
		t.Fatalf("unexpected monthly schedule %q", monthly.CompiledSchedule)
	}
}

func TestCompileReminderSpecPacificRejectsUnsupportedPatterns(t *testing.T) {
	now := time.Now()
	_, err := CompileReminderSpecPacific(tools.ReminderSpec{
		Mode: "weekdays",
		Days: []string{"abc"},
		Time: "09:00",
	}, now)
	if err == nil {
		t.Fatalf("expected invalid weekday rejection")
	}

	_, err = CompileReminderSpecPacific(tools.ReminderSpec{
		Mode:         "interval",
		IntervalUnit: "hour",
		Interval:     24,
	}, now)
	if err == nil {
		t.Fatalf("expected unsupported hour interval rejection")
	}
}

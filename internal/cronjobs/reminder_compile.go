package cronjobs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/tools"
	"github.com/ParthSareen/OllamaClaw/internal/util"
)

type CompiledReminder struct {
	Mode               string
	CompiledSchedule   string
	NormalizedSpecJSON string
	OnceFireAt         *time.Time
}

func CompileReminderSpecPacific(spec tools.ReminderSpec, now time.Time) (CompiledReminder, error) {
	nowPacific := now.In(util.PacificLocation())
	mode := strings.ToLower(strings.TrimSpace(spec.Mode))
	switch mode {
	case "once":
		return compileOnceReminder(spec, nowPacific)
	case "daily":
		return compileDailyReminder(spec)
	case "interval":
		return compileIntervalReminder(spec)
	case "weekdays":
		return compileWeekdaysReminder(spec)
	case "monthly":
		return compileMonthlyReminder(spec)
	default:
		return CompiledReminder{}, fmt.Errorf("unsupported reminder mode %q", spec.Mode)
	}
}

func compileDailyReminder(spec tools.ReminderSpec) (CompiledReminder, error) {
	hour, minute, err := parseTimeHHMM(spec.Time)
	if err != nil {
		return CompiledReminder{}, fmt.Errorf("daily mode requires valid time HH:MM in %s: %w", util.PacificTimezoneName, err)
	}
	normalized := map[string]interface{}{
		"mode": "daily",
		"time": fmt.Sprintf("%02d:%02d", hour, minute),
	}
	specJSON, err := mustMarshalJSON(normalized)
	if err != nil {
		return CompiledReminder{}, err
	}
	return CompiledReminder{
		Mode:               "daily",
		CompiledSchedule:   fmt.Sprintf("%d %d * * *", minute, hour),
		NormalizedSpecJSON: specJSON,
	}, nil
}

func compileOnceReminder(spec tools.ReminderSpec, nowPacific time.Time) (CompiledReminder, error) {
	date := strings.TrimSpace(spec.Date)
	timeText := strings.TrimSpace(spec.Time)
	if date == "" || timeText == "" {
		return CompiledReminder{}, fmt.Errorf("once mode requires date and time")
	}
	fireAt, err := parseDateTimePacific(date, timeText)
	if err != nil {
		return CompiledReminder{}, err
	}
	if !fireAt.After(nowPacific) {
		return CompiledReminder{}, fmt.Errorf("once reminder datetime must be in the future (%s)", util.PacificTimezoneName)
	}
	normalized := map[string]interface{}{
		"mode": "once",
		"date": fireAt.Format("2006-01-02"),
		"time": fireAt.Format("15:04"),
	}
	specJSON, err := mustMarshalJSON(normalized)
	if err != nil {
		return CompiledReminder{}, err
	}
	return CompiledReminder{
		Mode:               "once",
		CompiledSchedule:   fmt.Sprintf("%d %d %d %d *", fireAt.Minute(), fireAt.Hour(), fireAt.Day(), int(fireAt.Month())),
		NormalizedSpecJSON: specJSON,
		OnceFireAt:         &fireAt,
	}, nil
}

func compileIntervalReminder(spec tools.ReminderSpec) (CompiledReminder, error) {
	unit := strings.ToLower(strings.TrimSpace(spec.IntervalUnit))
	interval := spec.Interval
	if interval <= 0 {
		return CompiledReminder{}, fmt.Errorf("interval mode requires interval >= 1")
	}
	switch unit {
	case "minute":
		if interval > 59 {
			return CompiledReminder{}, fmt.Errorf("interval minute must be <= 59")
		}
		normalized := map[string]interface{}{
			"mode":          "interval",
			"interval_unit": "minute",
			"interval":      interval,
		}
		specJSON, err := mustMarshalJSON(normalized)
		if err != nil {
			return CompiledReminder{}, err
		}
		return CompiledReminder{
			Mode:               "interval",
			CompiledSchedule:   fmt.Sprintf("*/%d * * * *", interval),
			NormalizedSpecJSON: specJSON,
		}, nil
	case "hour":
		if interval > 23 {
			return CompiledReminder{}, fmt.Errorf("interval hour must be <= 23")
		}
		minute := 0
		if spec.Minute != nil {
			minute = *spec.Minute
		}
		if minute < 0 || minute > 59 {
			return CompiledReminder{}, fmt.Errorf("interval hour minute must be between 0 and 59")
		}
		normalized := map[string]interface{}{
			"mode":          "interval",
			"interval_unit": "hour",
			"interval":      interval,
			"minute":        minute,
		}
		specJSON, err := mustMarshalJSON(normalized)
		if err != nil {
			return CompiledReminder{}, err
		}
		return CompiledReminder{
			Mode:               "interval",
			CompiledSchedule:   fmt.Sprintf("%d */%d * * *", minute, interval),
			NormalizedSpecJSON: specJSON,
		}, nil
	case "day":
		if interval > 31 {
			return CompiledReminder{}, fmt.Errorf("interval day must be <= 31")
		}
		hour, minute, err := parseTimeHHMM(spec.Time)
		if err != nil {
			return CompiledReminder{}, fmt.Errorf("interval day requires valid time HH:MM in %s: %w", util.PacificTimezoneName, err)
		}
		normalized := map[string]interface{}{
			"mode":          "interval",
			"interval_unit": "day",
			"interval":      interval,
			"time":          fmt.Sprintf("%02d:%02d", hour, minute),
		}
		specJSON, err := mustMarshalJSON(normalized)
		if err != nil {
			return CompiledReminder{}, err
		}
		return CompiledReminder{
			Mode:               "interval",
			CompiledSchedule:   fmt.Sprintf("%d %d */%d * *", minute, hour, interval),
			NormalizedSpecJSON: specJSON,
		}, nil
	default:
		return CompiledReminder{}, fmt.Errorf("interval mode supports only minute, hour, or day units")
	}
}

func compileWeekdaysReminder(spec tools.ReminderSpec) (CompiledReminder, error) {
	hour, minute, err := parseTimeHHMM(spec.Time)
	if err != nil {
		return CompiledReminder{}, fmt.Errorf("weekdays mode requires valid time HH:MM in %s: %w", util.PacificTimezoneName, err)
	}
	days, cronDOW, err := normalizeWeekdays(spec.Days)
	if err != nil {
		return CompiledReminder{}, err
	}
	normalized := map[string]interface{}{
		"mode": "weekdays",
		"days": days,
		"time": fmt.Sprintf("%02d:%02d", hour, minute),
	}
	specJSON, err := mustMarshalJSON(normalized)
	if err != nil {
		return CompiledReminder{}, err
	}
	return CompiledReminder{
		Mode:               "weekdays",
		CompiledSchedule:   fmt.Sprintf("%d %d * * %s", minute, hour, cronDOW),
		NormalizedSpecJSON: specJSON,
	}, nil
}

func compileMonthlyReminder(spec tools.ReminderSpec) (CompiledReminder, error) {
	day := spec.DayOfMonth
	if day < 1 || day > 31 {
		return CompiledReminder{}, fmt.Errorf("monthly mode requires day_of_month between 1 and 31")
	}
	hour, minute, err := parseTimeHHMM(spec.Time)
	if err != nil {
		return CompiledReminder{}, fmt.Errorf("monthly mode requires valid time HH:MM in %s: %w", util.PacificTimezoneName, err)
	}
	normalized := map[string]interface{}{
		"mode":         "monthly",
		"day_of_month": day,
		"time":         fmt.Sprintf("%02d:%02d", hour, minute),
	}
	specJSON, err := mustMarshalJSON(normalized)
	if err != nil {
		return CompiledReminder{}, err
	}
	return CompiledReminder{
		Mode:               "monthly",
		CompiledSchedule:   fmt.Sprintf("%d %d %d * *", minute, hour, day),
		NormalizedSpecJSON: specJSON,
	}, nil
}

func parseDateTimePacific(date, timeText string) (time.Time, error) {
	loc := util.PacificLocation()
	combined := strings.TrimSpace(date) + " " + strings.TrimSpace(timeText)
	t, err := time.ParseInLocation("2006-01-02 15:04", combined, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date/time; expected YYYY-MM-DD and HH:MM in %s", util.PacificTimezoneName)
	}
	return t, nil
}

func parseTimeHHMM(raw string) (int, int, error) {
	text := strings.TrimSpace(raw)
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time %q", raw)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", raw)
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", raw)
	}
	return hour, minute, nil
}

func normalizeWeekdays(rawDays []string) ([]string, string, error) {
	if len(rawDays) == 0 {
		return nil, "", fmt.Errorf("weekdays mode requires at least one day")
	}
	nameToNum := map[string]int{
		"sun": 0,
		"mon": 1,
		"tue": 2,
		"wed": 3,
		"thu": 4,
		"fri": 5,
		"sat": 6,
	}
	numToName := map[int]string{
		0: "sun",
		1: "mon",
		2: "tue",
		3: "wed",
		4: "thu",
		5: "fri",
		6: "sat",
	}
	seen := map[int]struct{}{}
	nums := make([]int, 0, len(rawDays))
	for _, item := range rawDays {
		day := strings.ToLower(strings.TrimSpace(item))
		n, ok := nameToNum[day]
		if !ok {
			return nil, "", fmt.Errorf("unsupported weekday %q (use mon,tue,wed,thu,fri,sat,sun)", item)
		}
		if _, exists := seen[n]; exists {
			continue
		}
		seen[n] = struct{}{}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil, "", fmt.Errorf("weekdays mode requires at least one valid day")
	}
	sort.Ints(nums)
	names := make([]string, 0, len(nums))
	cronParts := make([]string, 0, len(nums))
	for _, n := range nums {
		names = append(names, numToName[n])
		cronParts = append(cronParts, strconv.Itoa(n))
	}
	return names, strings.Join(cronParts, ","), nil
}

func mustMarshalJSON(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal normalized reminder spec: %w", err)
	}
	return string(b), nil
}

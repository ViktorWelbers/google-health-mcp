package main

import (
	"sort"
	"strings"
	"time"
)

// KnownDataTypes is the full v4 data type list, kebab-case as the API expects
// it in the URL path.
var KnownDataTypes = []string{
	"active-energy-burned", "active-zone-minutes", "activity-level", "altitude",
	"basal-energy-burned", "blood-glucose", "body-fat", "calories-in-heart-rate-zone",
	"core-body-temperature", "daily-heart-rate-variability", "daily-oxygen-saturation",
	"daily-resting-heart-rate", "daily-sleep-temperature-derivations", "distance",
	"electrocardiogram", "exercise", "floors", "food", "food-measurement-unit",
	"heart-rate", "heart-rate-variability", "irregular-rhythm-notification",
	"nutrition-log", "oxygen-saturation", "run-vo2-max", "sedentary-period",
	"sleep", "steps", "swim-lengths-data", "total-calories", "vo2-max", "weight",
}

// Presets are named bundles of data types — a convenience so a caller can ask
// for "recovery" instead of listing four kebab-case strings. They group data;
// they do not interpret it.
var Presets = map[string][]string{
	"recovery": {
		"daily-resting-heart-rate",
		"daily-heart-rate-variability",
		"daily-sleep-temperature-derivations",
		"daily-oxygen-saturation",
		"sleep",
	},
	"training": {
		"active-zone-minutes",
		"active-energy-burned",
		"total-calories",
		"steps",
		"exercise",
	},
	"body": {
		"weight",
		"body-fat",
		"vo2-max",
		"blood-glucose",
	},
}

func presetNames() []string {
	names := make([]string, 0, len(Presets))
	for k := range Presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// preferredKeys nudges value extraction toward the right field for types whose
// rollup object carries several numbers. Purely mechanical — it decides which
// number is "the" value, not what the value means.
var preferredKeys = map[string][]string{
	"daily-resting-heart-rate":            {"restingHeartRate", "bpm", "value"},
	"daily-heart-rate-variability":        {"rmssd", "dailyRmssd", "value"},
	"daily-sleep-temperature-derivations": {"temperatureDeviationCelsius", "deviationCelsius", "celsius", "value"},
	"daily-oxygen-saturation":             {"averagePercentage", "percentage", "value"},
	"sleep":                               {"totalSleepMinutes", "durationMinutes", "minutes"},
	"weight":                              {"weightKilograms", "kilograms", "value"},
	"steps":                               {"count", "steps", "value"},
}

// ---------- extraction ----------

// dayOf pulls the calendar date out of a rollup point's civilStartTime.
func dayOf(point map[string]any) (time.Time, bool) {
	for _, key := range []string{"civilStartTime", "startTime", "civilEndTime"} {
		node, ok := point[key].(map[string]any)
		if !ok {
			continue
		}
		date, ok := node["date"].(map[string]any)
		if !ok {
			continue
		}
		y, ok1 := date["year"].(float64)
		m, ok2 := date["month"].(float64)
		d, ok3 := date["day"].(float64)
		if ok1 && ok2 && ok3 {
			return time.Date(int(y), time.Month(int(m)), int(d), 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

// structuralKeys hold numbers but are never the metric itself.
var structuralKeys = map[string]bool{
	"year": true, "month": true, "day": true, "hours": true,
	"minutes": true, "seconds": true, "nanos": true,
	"civilStartTime": true, "civilEndTime": true, "dataSource": true,
}

// extractValue finds the metric number inside a rollup point.
//
// The API returns a different value object per data type and the nested field
// names are not exhaustively documented, so this tries preferred keys first and
// otherwise takes the first plausible number. Being tolerant here is what lets
// all 30+ data types work without per-type code.
func extractValue(point map[string]any, prefer []string) (float64, bool) {
	for _, key := range prefer {
		if v, ok := searchKey(point, key); ok {
			return v, true
		}
	}
	return firstNumber(point)
}

func searchKey(node any, want string) (float64, bool) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if strings.EqualFold(k, want) {
				if f, ok := asNumber(v); ok {
					return f, true
				}
			}
		}
		// Descend only after checking this level, so shallower matches win.
		for k, v := range n {
			if structuralKeys[k] {
				continue
			}
			if f, ok := searchKey(v, want); ok {
				return f, true
			}
		}
	case []any:
		for _, v := range n {
			if f, ok := searchKey(v, want); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func firstNumber(node any) (float64, bool) {
	switch n := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			if !structuralKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys) // deterministic
		for _, k := range keys {
			if f, ok := asNumber(n[k]); ok {
				return f, true
			}
		}
		for _, k := range keys {
			if f, ok := firstNumber(n[k]); ok {
				return f, true
			}
		}
	case []any:
		for _, v := range n {
			if f, ok := firstNumber(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// ---------- series ----------

type dayValue struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// Series is one data type's day-by-day values over the requested window.
// No baselines, no thresholds, no verdicts — the caller does the thinking.
type Series struct {
	DataType string     `json:"data_type"`
	Days     []dayValue `json:"days"`
	Count    int        `json:"count"`
	Error    string     `json:"error,omitempty"`
}

func newSeries(dataType string, points []map[string]any) Series {
	s := Series{DataType: dataType}
	for _, p := range points {
		d, ok := dayOf(p)
		if !ok {
			continue
		}
		v, ok := extractValue(p, preferredKeys[dataType])
		if !ok {
			continue
		}
		s.Days = append(s.Days, dayValue{Date: d.Format("2006-01-02"), Value: v})
	}
	sort.Slice(s.Days, func(i, j int) bool { return s.Days[i].Date < s.Days[j].Date })
	s.Count = len(s.Days)
	return s
}

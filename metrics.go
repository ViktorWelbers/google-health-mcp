package main

import (
	"sort"
	"strconv"
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
// for "recovery" instead of listing kebab-case strings. They group data; they
// do not interpret it.
var Presets = map[string][]string{
	"recovery": {
		"daily-resting-heart-rate",
		"daily-heart-rate-variability",
		"daily-sleep-temperature-derivations",
		"daily-oxygen-saturation",
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

// primaryField names the value shown in the rendered table for each data type.
// Confirmed against live API payloads. This only chooses a display column —
// every numeric field is returned in structured output regardless, so a wrong
// or missing entry here loses nothing.
var primaryField = map[string]string{
	"daily-resting-heart-rate":            "beatsPerMinute",
	"daily-heart-rate-variability":        "averageHeartRateVariabilityMilliseconds",
	"daily-oxygen-saturation":             "averagePercentage",
	"daily-sleep-temperature-derivations": "nightlyTemperatureCelsius",
	"weight":                              "weightGrams",
	"body-fat":                            "percentage",
	"steps":                               "count",
}

// ---------- extraction ----------

// structuralKeys hold numbers that describe position in time, never a metric.
var structuralKeys = map[string]bool{
	"year": true, "month": true, "day": true, "hours": true,
	"minutes": true, "seconds": true, "nanos": true, "utcOffset": true,
}

// asNumber accepts JSON numbers and numeric strings. The API returns some
// integer fields quoted — daily-resting-heart-rate reports "beatsPerMinute":"59".
func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// dayOf finds the calendar date for a data point.
//
// Placement varies: rollup points carry civilStartTime at the top level, daily
// types nest date inside their value object (dailyRestingHeartRate.date), and
// sampled types bury it deeper still (weight.sampleTime.civilTime.date). So
// this searches recursively for the first object holding year/month/day.
func dayOf(node any) (time.Time, bool) {
	switch n := node.(type) {
	case map[string]any:
		y, ok1 := asNumber(n["year"])
		m, ok2 := asNumber(n["month"])
		d, ok3 := asNumber(n["day"])
		if ok1 && ok2 && ok3 {
			return time.Date(int(y), time.Month(int(m)), int(d), 0, 0, 0, 0, time.UTC), true
		}
		// Prefer an explicit date/civilStartTime branch before scanning the rest.
		for _, k := range []string{"date", "civilStartTime", "civilTime", "sampleTime", "startTime"} {
			if v, ok := n[k]; ok {
				if t, ok := dayOf(v); ok {
					return t, true
				}
			}
		}
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if t, ok := dayOf(n[k]); ok {
				return t, true
			}
		}
	case []any:
		for _, v := range n {
			if t, ok := dayOf(v); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// flattenNumbers collects every numeric leaf in a data point, keyed by its own
// field name. Returning all of them removes any need to guess which field
// "the" value is — the caller sees the lot.
func flattenNumbers(node any, out map[string]float64) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if structuralKeys[k] || k == "dataSource" {
				continue
			}
			if f, ok := asNumber(v); ok {
				// Keep the shallowest occurrence of a name.
				if _, exists := out[k]; !exists {
					out[k] = f
				}
				continue
			}
			flattenNumbers(v, out)
		}
	case []any:
		for _, v := range n {
			flattenNumbers(v, out)
		}
	}
}

// ---------- series ----------

type dayValue struct {
	Date   string             `json:"date"`
	Value  float64            `json:"value"`
	Field  string             `json:"field"`
	Values map[string]float64 `json:"values,omitempty"`
}

// Series is one data type's day-by-day values over the requested window.
// No baselines, no thresholds, no verdicts — the caller does the thinking.
type Series struct {
	DataType string     `json:"data_type"`
	Field    string     `json:"primary_field,omitempty"`
	Days     []dayValue `json:"days"`
	Count    int        `json:"count"`
	Error    string     `json:"error,omitempty"`
}

func newSeries(dataType string, points []map[string]any) Series {
	s := Series{DataType: dataType, Field: primaryField[dataType]}
	for _, p := range points {
		d, ok := dayOf(p)
		if !ok {
			continue
		}
		nums := map[string]float64{}
		flattenNumbers(p, nums)
		if len(nums) == 0 {
			continue
		}
		dv := dayValue{Date: d.Format("2006-01-02"), Values: nums}

		// Pick the display value: configured primary if present, else the
		// alphabetically first field so the choice is at least deterministic.
		if v, ok := nums[s.Field]; ok {
			dv.Value, dv.Field = v, s.Field
		} else {
			keys := make([]string, 0, len(nums))
			for k := range nums {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			dv.Value, dv.Field = nums[keys[0]], keys[0]
		}
		s.Days = append(s.Days, dv)
	}
	sort.Slice(s.Days, func(i, j int) bool { return s.Days[i].Date < s.Days[j].Date })
	s.Count = len(s.Days)
	if s.Field == "" && len(s.Days) > 0 {
		s.Field = s.Days[0].Field
	}
	return s
}

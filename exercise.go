package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Workout is one recorded activity, flattened from the exercise data type.
//
// Exercise points carry a much richer payload than the single number the daily
// metrics tool deals in — type, duration, distance, elevation, heart-rate zones
// — so they get their own shape rather than being squeezed into a date/value
// pair. Units are converted to the ones a human reads (km, m, minutes); the raw
// payload uses millimetres and seconds.
type Workout struct {
	Date        string  `json:"date"`
	Start       string  `json:"start,omitempty"`
	Type        string  `json:"type"`
	Name        string  `json:"name,omitempty"`
	DurationMin float64 `json:"duration_min"`
	DistanceKm  float64 `json:"distance_km,omitempty"`
	ElevationM  float64 `json:"elevation_m,omitempty"`
	AvgHR       float64 `json:"avg_hr,omitempty"`
	Calories    float64 `json:"calories,omitempty"`
	ActiveZone  float64 `json:"active_zone_min,omitempty"`
	Steps       float64 `json:"steps,omitempty"`
}

// num pulls a number out of a map, tolerating the API's habit of quoting
// integers ("steps": "16595").
func num(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := asNumber(v); ok {
			return f
		}
	}
	return 0
}

// parseDuration reads the protobuf duration format ("12676s").
func parseDuration(v any) float64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil {
		return 0
	}
	return f
}

func workoutFrom(point map[string]any) (Workout, bool) {
	ex, ok := point["exercise"].(map[string]any)
	if !ok {
		return Workout{}, false
	}
	w := Workout{}

	if iv, ok := ex["interval"].(map[string]any); ok {
		if st, ok := iv["startTime"].(string); ok {
			if t, err := time.Parse(time.RFC3339, st); err == nil {
				w.Date = t.Format("2006-01-02")
				w.Start = t.Format("15:04")
			}
		}
	}
	if t, ok := ex["exerciseType"].(string); ok {
		w.Type = t
	}
	if n, ok := ex["displayName"].(string); ok {
		w.Name = n
	}
	w.DurationMin = parseDuration(ex["activeDuration"]) / 60

	if ms, ok := ex["metricsSummary"].(map[string]any); ok {
		w.DistanceKm = num(ms, "distanceMillimeters") / 1_000_000
		w.ElevationM = num(ms, "elevationGainMillimeters") / 1000
		w.AvgHR = num(ms, "averageHeartRateBeatsPerMinute")
		w.Calories = num(ms, "caloriesKcal")
		w.ActiveZone = num(ms, "activeZoneMinutes")
		w.Steps = num(ms, "steps")
	}
	return w, w.Date != ""
}

// ---------- health_workouts ----------

type workoutArgs struct {
	Days  int `json:"days,omitempty" jsonschema:"days of history ending today, default 30"`
	Limit int `json:"limit,omitempty" jsonschema:"maximum workouts to return, default 25"`
}

type workoutOut struct {
	Window   string    `json:"window"`
	Count    int       `json:"count"`
	Workouts []Workout `json:"workouts"`
}

// workouts lists every recorded activity, not just cycling. Walks, hikes and
// gym sessions are real training load that a cycling-only view misses entirely
// — a 900 m hike is not a rest day.
func (a *app) workouts(ctx context.Context, _ *mcp.CallToolRequest, in workoutArgs) (*mcp.CallToolResult, workoutOut, error) {
	if in.Days <= 0 {
		in.Days = 30
	}
	if in.Limit <= 0 {
		in.Limit = 25
	}
	c, err := a.apiClient(ctx)
	if err != nil {
		return nil, workoutOut{}, err
	}

	// The API returns newest first; fetch a generous page and filter by date.
	points, err := c.List(ctx, "exercise", "", in.Limit*2, 2)
	if err != nil {
		return nil, workoutOut{}, err
	}

	cutoff := time.Now().AddDate(0, 0, -in.Days).Format("2006-01-02")
	out := workoutOut{Window: fmt.Sprintf("%s → %s", cutoff, time.Now().Format("2006-01-02"))}
	for _, p := range points {
		w, ok := workoutFrom(p)
		if !ok || w.Date < cutoff {
			continue
		}
		out.Workouts = append(out.Workouts, w)
	}
	sort.Slice(out.Workouts, func(i, j int) bool {
		if out.Workouts[i].Date != out.Workouts[j].Date {
			return out.Workouts[i].Date < out.Workouts[j].Date
		}
		return out.Workouts[i].Start < out.Workouts[j].Start
	})
	if len(out.Workouts) > in.Limit {
		out.Workouts = out.Workouts[len(out.Workouts)-in.Limit:]
	}
	out.Count = len(out.Workouts)

	return textResult(renderWorkouts(out)), out, nil
}

func renderWorkouts(out workoutOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d workouts\n\n", out.Window, out.Count)
	if out.Count == 0 {
		b.WriteString("No recorded activities in this window.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-11s %-6s %-14s %7s %8s %7s %6s %7s %5s\n",
		"date", "start", "type", "min", "km", "elev m", "avgHR", "kcal", "AZM")
	for _, w := range out.Workouts {
		name := w.Type
		if name == "" {
			name = w.Name
		}
		if len(name) > 14 {
			name = name[:14]
		}
		fmt.Fprintf(&b, "%-11s %-6s %-14s %7.0f %8.2f %7.0f %6.0f %7.0f %5.0f\n",
			w.Date, w.Start, name, w.DurationMin, w.DistanceKm, w.ElevationM, w.AvgHR, w.Calories, w.ActiveZone)
	}
	return b.String()
}

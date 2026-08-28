package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// app holds lazily-initialised state. The client is built on first use rather
// than at startup so the server still boots (and reports a useful error) when
// it has not been authorised yet — important when it lives in a cluster.
type app struct {
	mu     sync.Mutex
	client *Client
}

func (a *app) apiClient(ctx context.Context) (*Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	hc, err := httpClient(ctx)
	if err != nil {
		return nil, err
	}
	a.client = NewClient(hc)
	return a.client, nil
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// ---------- health_auth_status ----------

type authArgs struct{}

type authOut struct {
	Authorized bool     `json:"authorized"`
	TokenPath  string   `json:"token_path"`
	Expiry     string   `json:"expiry,omitempty"`
	HasRefresh bool     `json:"has_refresh_token"`
	Scopes     []string `json:"scopes"`
	Detail     string   `json:"detail,omitempty"`
}

func (a *app) authStatus(_ context.Context, _ *mcp.CallToolRequest, _ authArgs) (*mcp.CallToolResult, authOut, error) {
	out := authOut{TokenPath: tokenPath(), Scopes: scopes}
	tok, err := loadToken()
	if err != nil {
		out.Detail = fmt.Sprintf("not authorized: %v — run `health-mcp login`", err)
		return textResult(out.Detail), out, nil
	}
	out.Authorized = tok.Valid() || tok.RefreshToken != ""
	out.HasRefresh = tok.RefreshToken != ""
	if !tok.Expiry.IsZero() {
		out.Expiry = tok.Expiry.Format(time.RFC3339)
	}
	switch {
	case tok.Valid():
		out.Detail = "access token valid"
	case out.HasRefresh:
		out.Detail = "access token expired but a refresh token is present — it will renew automatically"
	default:
		out.Detail = "access token expired and no refresh token — re-run `health-mcp login`"
	}
	return textResult(fmt.Sprintf("%s\nToken: %s\nExpiry: %s", out.Detail, out.TokenPath, out.Expiry)), out, nil
}

// ---------- health_data_types ----------

type typesArgs struct{}

type typesOut struct {
	DataTypes []string            `json:"data_types"`
	Presets   map[string][]string `json:"presets"`
}

func (a *app) dataTypes(_ context.Context, _ *mcp.CallToolRequest, _ typesArgs) (*mcp.CallToolResult, typesOut, error) {
	out := typesOut{DataTypes: KnownDataTypes, Presets: Presets}
	var b strings.Builder
	b.WriteString("Presets:\n")
	for _, name := range presetNames() {
		fmt.Fprintf(&b, "  %-10s %s\n", name, strings.Join(Presets[name], ", "))
	}
	b.WriteString("\nAll data types:\n  " + strings.Join(KnownDataTypes, "\n  ") + "\n")
	return textResult(b.String()), out, nil
}

// ---------- health_daily_metrics ----------

type dailyArgs struct {
	DataTypes []string `json:"data_types,omitempty" jsonschema:"kebab-case data types to fetch, e.g. [daily-resting-heart-rate, sleep]"`
	Preset    string   `json:"preset,omitempty" jsonschema:"named bundle instead of data_types: recovery, training or body"`
	Days      int      `json:"days,omitempty" jsonschema:"days of history ending today, default 30"`
}

type dailyOut struct {
	Window string   `json:"window"`
	Series []Series `json:"series"`
}

// dailyMetrics fetches day-by-day values for one or more data types in a single
// call. It returns numbers and nothing else — no baselines, no flags, no
// verdicts. Interpretation is the caller's job, and the caller has context the
// server does not (training load, symptoms, what happened last week).
func (a *app) dailyMetrics(ctx context.Context, _ *mcp.CallToolRequest, in dailyArgs) (*mcp.CallToolResult, dailyOut, error) {
	types := in.DataTypes
	if in.Preset != "" {
		p, ok := Presets[in.Preset]
		if !ok {
			return nil, dailyOut{}, fmt.Errorf("unknown preset %q — available: %s", in.Preset, strings.Join(presetNames(), ", "))
		}
		types = append(types, p...)
	}
	if len(types) == 0 {
		return nil, dailyOut{}, fmt.Errorf("provide data_types or preset — call health_data_types to see the options")
	}
	if in.Days <= 0 {
		in.Days = 30
	}

	c, err := a.apiClient(ctx)
	if err != nil {
		return nil, dailyOut{}, err
	}

	end := time.Now().AddDate(0, 0, 1) // exclusive end — include today
	start := end.AddDate(0, 0, -in.Days)

	out := dailyOut{Window: fmt.Sprintf("%s → %s", start.Format("2006-01-02"), end.AddDate(0, 0, -1).Format("2006-01-02"))}
	for _, dt := range types {
		points, err := c.Fetch(ctx, dt, start, end, in.Days)
		if err != nil {
			out.Series = append(out.Series, Series{DataType: dt, Error: err.Error()})
			continue
		}
		out.Series = append(out.Series, newSeries(dt, points))
	}

	return textResult(renderDaily(out)), out, nil
}

// renderDaily lays the series out as aligned columns, which is far cheaper to
// read than repeated JSON and keeps every raw number visible.
func renderDaily(out dailyOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", out.Window)

	// Union of dates across all series, so everything lines up by day.
	dateSet := map[string]bool{}
	for _, s := range out.Series {
		for _, d := range s.Days {
			dateSet[d.Date] = true
		}
	}
	if len(dateSet) == 0 {
		for _, s := range out.Series {
			if s.Error != "" {
				fmt.Fprintf(&b, "%s: %s\n", s.DataType, s.Error)
			} else {
				fmt.Fprintf(&b, "%s: no data in window\n", s.DataType)
			}
		}
		return b.String()
	}
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	// Header: data type on one line, the field supplying the column on the next,
	// so the unit is never ambiguous.
	fmt.Fprintf(&b, "%-12s", "date")
	for _, s := range out.Series {
		fmt.Fprintf(&b, " %16s", shortName(s.DataType))
	}
	fmt.Fprintf(&b, "\n%-12s", "")
	for _, s := range out.Series {
		fmt.Fprintf(&b, " %16s", shortField(s.Field))
	}
	b.WriteString("\n")

	index := make([]map[string]float64, len(out.Series))
	for i, s := range out.Series {
		m := map[string]float64{}
		for _, d := range s.Days {
			m[d.Date] = d.Value
		}
		index[i] = m
	}
	for _, date := range dates {
		fmt.Fprintf(&b, "%-12s", date)
		for i := range out.Series {
			if v, ok := index[i][date]; ok {
				fmt.Fprintf(&b, " %16.2f", v)
			} else {
				fmt.Fprintf(&b, " %16s", "—")
			}
		}
		b.WriteString("\n")
	}
	for _, s := range out.Series {
		if s.Error != "" {
			fmt.Fprintf(&b, "\n%s: %s", s.DataType, s.Error)
		}
	}
	return b.String()
}

func shortName(dt string) string {
	s := strings.TrimPrefix(dt, "daily-")
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}

// shortField trims the long descriptive field names the API uses
// (averageHeartRateVariabilityMilliseconds) down to something a column can hold,
// keeping the tail because that is where the unit lives.
func shortField(f string) string {
	if f == "" {
		return ""
	}
	if len(f) > 16 {
		return "…" + f[len(f)-15:]
	}
	return f
}

// ---------- health_list_datapoints ----------

type listArgs struct {
	DataType string `json:"data_type" jsonschema:"kebab-case data type"`
	Filter   string `json:"filter,omitempty" jsonschema:"optional AIP-160 filter expression passed to the API untouched"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"points per page, default 100"`
	MaxPages int    `json:"max_pages,omitempty" jsonschema:"pages to fetch, default 1"`
}

type listOut struct {
	DataType string          `json:"data_type"`
	Count    int             `json:"count"`
	Raw      json.RawMessage `json:"raw"`
}

// listDatapoints is the escape hatch: raw API access with a pass-through
// filter, so new data types, intraday detail or filter syntax can be explored
// without changing the server.
func (a *app) listDatapoints(ctx context.Context, _ *mcp.CallToolRequest, in listArgs) (*mcp.CallToolResult, listOut, error) {
	if in.DataType == "" {
		return nil, listOut{}, fmt.Errorf("data_type is required")
	}
	if in.PageSize <= 0 {
		in.PageSize = 100
	}
	if in.MaxPages <= 0 {
		in.MaxPages = 1
	}
	c, err := a.apiClient(ctx)
	if err != nil {
		return nil, listOut{}, err
	}
	points, err := c.List(ctx, in.DataType, in.Filter, in.PageSize, in.MaxPages)
	if err != nil {
		return nil, listOut{}, err
	}
	raw, err := json.MarshalIndent(points, "", "  ")
	if err != nil {
		return nil, listOut{}, err
	}
	// Keep the text channel bounded; the full payload stays in structured output.
	preview := string(raw)
	if len(preview) > 6000 {
		preview = preview[:6000] + "\n… truncated, full payload in structured output"
	}
	out := listOut{DataType: in.DataType, Count: len(points), Raw: raw}
	return textResult(fmt.Sprintf("%s — %d data points\n%s", in.DataType, len(points), preview)), out, nil
}

// register wires every tool onto the server.
func (a *app) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "health_auth_status",
		Description: "Check whether the server is authorised against the Google Health API, where the token lives, when it expires, and which scopes are granted. Call this first if other tools fail.",
	}, a.authStatus)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "health_data_types",
		Description: "List every Google Health API v4 data type in the kebab-case form the other tools expect, plus the named presets.",
	}, a.dataTypes)

	mcp.AddTool(s, &mcp.Tool{
		Name: "health_daily_metrics",
		Description: "Day-by-day values for one or more data types over a date range, returned as a plain date/value table. " +
			"Use preset 'recovery' for resting heart rate, HRV, overnight skin-temperature deviation, SpO2 and sleep — the signals that move when fighting an infection or carrying heavy fatigue. " +
			"Returns raw numbers only; draw your own conclusions from them.",
	}, a.dailyMetrics)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "health_list_datapoints",
		Description: "Raw data point access with an optional pass-through AIP-160 filter. Escape hatch for intraday detail or queries the daily tool does not cover.",
	}, a.listDatapoints)
}

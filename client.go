package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBase = "https://health.googleapis.com/v4"

// Client is a thin wrapper over the Google Health API v4 REST surface.
// It deliberately decodes payloads into generic maps: the API has 30+ data
// types with different value shapes, and staying schema-agnostic means a new
// data type never requires a code change.
type Client struct {
	hc   *http.Client
	base string
}

func NewClient(hc *http.Client) *Client {
	return &Client{hc: hc, base: defaultBase}
}

// apiError surfaces Google's own error message rather than a bare status code,
// which matters a lot when debugging filter expressions and scopes.
type apiError struct {
	Status int
	Body   string
	URL    string
}

func (e *apiError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 800 {
		body = body[:800] + "…"
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("401 unauthorized — token expired or revoked; run `health-mcp login` again. %s", body)
	case http.StatusForbidden:
		return fmt.Sprintf("403 forbidden — the Health API may not be enabled on the project, or a required scope was not granted. %s", body)
	case http.StatusNotFound:
		return fmt.Sprintf("404 not found — check the data type name. %s", body)
	}
	return fmt.Sprintf("google health api %d on %s: %s", e.Status, e.URL, body)
}

func (c *Client) do(ctx context.Context, method, u string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{Status: resp.StatusCode, Body: string(raw), URL: u}
	}
	return raw, nil
}

// ---------- civil time ----------

type civilDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

type civilDateTime struct {
	Date civilDate `json:"date"`
}

// civilTimeInterval is the closed-open [start, end) window used by rollup calls.
// Field names confirmed against the live API: "start"/"end", not the
// startTime/endTime that google.type.Interval uses.
type civilTimeInterval struct {
	Start civilDateTime `json:"start"`
	End   civilDateTime `json:"end"`
}

func toCivil(t time.Time) civilDateTime {
	return civilDateTime{Date: civilDate{Year: t.Year(), Month: int(t.Month()), Day: t.Day()}}
}

// ---------- list ----------

type listResponse struct {
	DataPoints    []map[string]any `json:"dataPoints"`
	NextPageToken string           `json:"nextPageToken"`
}

// List reads raw data points for a data type. filter is an AIP-160 expression
// (https://google.aip.dev/160) and is passed through untouched, which keeps the
// escape-hatch tool useful without needing a rebuild.
func (c *Client) List(ctx context.Context, dataType, filter string, pageSize int, maxPages int) ([]map[string]any, error) {
	var out []map[string]any
	token := ""
	if maxPages <= 0 {
		maxPages = 5
	}
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		if filter != "" {
			q.Set("filter", filter)
		}
		if pageSize > 0 {
			q.Set("pageSize", fmt.Sprint(pageSize))
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		u := fmt.Sprintf("%s/users/me/dataTypes/%s/dataPoints", c.base, url.PathEscape(dataType))
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		raw, err := c.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		var lr listResponse
		if err := json.Unmarshal(raw, &lr); err != nil {
			return nil, fmt.Errorf("decoding list response for %s: %w", dataType, err)
		}
		out = append(out, lr.DataPoints...)
		if lr.NextPageToken == "" {
			break
		}
		token = lr.NextPageToken
	}
	return out, nil
}

// ---------- rollup ----------

type dailyRollUpRequest struct {
	Range          civilTimeInterval `json:"range"`
	WindowSizeDays int               `json:"windowSizeDays,omitempty"`
	PageSize       int               `json:"pageSize,omitempty"`
	PageToken      string            `json:"pageToken,omitempty"`
}

type dailyRollUpResponse struct {
	RollupDataPoints []map[string]any `json:"rollupDataPoints"`
	NextPageToken    string           `json:"nextPageToken"`
}

// maxRollupWindow is the API's documented ceiling for the busiest data types
// (heart-rate, active-minutes, total-calories, calories-in-heart-rate-zone).
// We chunk every request to stay under it rather than special-casing types.
const maxRollupWindow = 14 * 24 * time.Hour

// SupportsRollup reports whether a data type can be aggregated server-side.
// Types prefixed "daily-" are already one point per day and the API rejects
// dailyRollUp on them ("supported: list, reconcile"), so they are read with
// List instead.
func SupportsRollup(dataType string) bool {
	return !strings.HasPrefix(dataType, "daily-")
}

// Fetch reads a data type over a window using whichever method the API
// supports for it. For daily types it lists the most recent `days` points,
// which the API returns newest-first.
func (c *Client) Fetch(ctx context.Context, dataType string, start, end time.Time, days int) ([]map[string]any, error) {
	if SupportsRollup(dataType) {
		return c.DailyRollUp(ctx, dataType, start, end)
	}
	if days <= 0 {
		days = 30
	}
	return c.List(ctx, dataType, "", days, 1)
}

// DailyRollUp returns one aggregated point per day over [start, end).
// Requests longer than 14 days are split automatically and stitched back
// together, so callers can just ask for "the last 90 days".
func (c *Client) DailyRollUp(ctx context.Context, dataType string, start, end time.Time) ([]map[string]any, error) {
	var out []map[string]any
	for winStart := start; winStart.Before(end); {
		winEnd := winStart.Add(maxRollupWindow)
		if winEnd.After(end) {
			winEnd = end
		}
		points, err := c.rollupWindow(ctx, dataType, winStart, winEnd)
		if err != nil {
			return nil, err
		}
		out = append(out, points...)
		winStart = winEnd
	}
	return out, nil
}

func (c *Client) rollupWindow(ctx context.Context, dataType string, start, end time.Time) ([]map[string]any, error) {
	u := fmt.Sprintf("%s/users/me/dataTypes/%s/dataPoints:dailyRollUp", c.base, url.PathEscape(dataType))
	var out []map[string]any
	token := ""
	for page := 0; page < 10; page++ {
		req := dailyRollUpRequest{
			Range:          civilTimeInterval{Start: toCivil(start), End: toCivil(end)},
			WindowSizeDays: 1,
			PageSize:       1000,
			PageToken:      token,
		}
		raw, err := c.do(ctx, http.MethodPost, u, req)
		if err != nil {
			return nil, err
		}
		var rr dailyRollUpResponse
		if err := json.Unmarshal(raw, &rr); err != nil {
			return nil, fmt.Errorf("decoding rollup response for %s: %w", dataType, err)
		}
		out = append(out, rr.RollupDataPoints...)
		if rr.NextPageToken == "" {
			break
		}
		token = rr.NextPageToken
	}
	return out, nil
}

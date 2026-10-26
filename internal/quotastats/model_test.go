package quotastats

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func ptr[T any](v T) *T { return &v }

func TestNormalizeSelectedWindow(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	response := pluginapi.QuotaFetchResponse{ServerTimeOffsetMs: 1000, Groups: []pluginapi.QuotaGroup{{DisplayName: "standard", Buckets: []pluginapi.QuotaBucket{
		{Window: "5h", RemainingFraction: .9, ResetTime: now.Add(time.Hour).Format(time.RFC3339)},
		{Window: "weekly", RemainingFraction: .25, ResetTime: now.Add(48 * time.Hour).Format(time.RFC3339)},
		{Window: "weekly", RemainingFraction: math.NaN()},
		{Window: "weekly", RemainingFraction: 1.1},
	}}}}
	s, ok := Normalize("codex", "auth-index", now, response)
	if !ok || s.Remaining != 25 || s.Window != "standard/weekly" || *s.PeriodHours != 168 || *s.ResetAt != now.Add(48*time.Hour).UnixMilli()-1000 {
		t.Fatalf("unexpected sample: %+v", s)
	}
	response.Groups[0].Buckets = []pluginapi.QuotaBucket{{Window: "rolling", RemainingFraction: 0, ResetTime: "unknown"}}
	s, ok = Normalize("codex", "auth-index", now, response)
	if !ok || s.Remaining != 0 || s.ResetAt != nil || s.PeriodHours != nil {
		t.Fatalf("zero/unknown metadata lost: %+v", s)
	}
	if _, ok = Normalize("codex", "auth-index", now, pluginapi.QuotaFetchResponse{}); ok {
		t.Fatal("invented sample without buckets")
	}
}

func TestPoolsRatesAndOverlappingHistory(t *testing.T) {
	now := int64(100) * hourMS
	samples := []Sample{
		{Provider: "codex", Account: "a", At: now - 2*hourMS, Remaining: 80},
		{Provider: "codex", Account: "a", At: now - hourMS, Remaining: 50},
		{Provider: "codex", Account: "a", At: now, Remaining: 40},
		{Provider: "codex", Account: "b", At: now - hourMS, Remaining: 90},
		{Provider: "codex", Account: "b", At: now, Remaining: 70},
	}
	p := Pools(samples, now, 3, 1)[0]
	if p.AccountCount != 2 || p.Capacity != 200 || p.Remaining != 110 || p.Burn == nil || *p.Burn != 40 || *p.Drain != 20 {
		t.Fatalf("unexpected pool: %+v", p)
	}
	want := []Point{{now - hourMS, 140}, {now, 110}}
	if !reflect.DeepEqual(p.Points, want) {
		t.Fatalf("points=%v want=%v", p.Points, want)
	}
	if p.Projection.Available || p.Projection.EmptyAt != nil || len(p.Projection.Points) != 0 {
		t.Fatal("projection invented resets")
	}
	// A one-hour lookback uses only the last interval, not the older faster drain.
	p = Pools(samples, now, 1, 1)[0]
	if *p.Burn != 30 || *p.Drain != 15 {
		t.Fatalf("lookback ignored: %+v", p)
	}
	samples = append(samples, Sample{Provider: "codex", Account: "c", At: now, Remaining: 30})
	p = Pools(samples, now, 3, 1)[0]
	if p.Burn != nil || p.Drain != nil || p.Remaining != 140 {
		t.Fatal("missing rate presented as zero")
	}
}

func TestRateResetRefillAndEvidenceBoundaries(t *testing.T) {
	now := int64(100) * hourMS
	for _, tc := range []struct {
		name          string
		before, after Sample
		known         bool
		want          float64
	}{
		{"five minutes", Sample{At: now - 300000, Remaining: 50}, Sample{At: now, Remaining: 45}, true, 60},
		{"too short", Sample{At: now - 299999, Remaining: 50}, Sample{At: now, Remaining: 45}, false, 0},
		{"zero burn", Sample{At: now - hourMS, Remaining: 45}, Sample{At: now, Remaining: 45}, true, 0},
		{"refill", Sample{At: now - hourMS, Remaining: 20}, Sample{At: now, Remaining: 80}, false, 0},
		{"reset changed", Sample{At: now - hourMS, Remaining: 80, ResetAt: ptr(now + hourMS)}, Sample{At: now, Remaining: 60, ResetAt: ptr(now + 2*hourMS)}, false, 0},
		{"window changed", Sample{At: now - hourMS, Remaining: 80, Window: "weekly"}, Sample{At: now, Remaining: 60, Window: "5h"}, false, 0},
		{"reset crossed", Sample{At: now - hourMS, Remaining: 80, ResetAt: ptr(now)}, Sample{At: now, Remaining: 60, ResetAt: ptr(now)}, false, 0},
		{"outside lookback", Sample{At: now - 3*hourMS - 1, Remaining: 80}, Sample{At: now, Remaining: 60}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate := Rate([]Sample{tc.before, tc.after}, now, 3)
			if (rate != nil) != tc.known || (rate != nil && math.Abs(*rate-tc.want) > 1e-9) {
				t.Fatalf("rate=%v known=%v want=%v", rate, tc.known, tc.want)
			}
		})
	}
	changedWindow := []Sample{
		{At: now - 2*hourMS, Remaining: 80, Window: "weekly"},
		{At: now - hourMS, Remaining: 60, Window: "weekly"},
		{At: now, Remaining: 50, Window: "5h"},
	}
	if Rate(changedWindow, now, 3) != nil {
		t.Fatal("rate from a different window applied to the current balance")
	}
}

func TestPoolsProjectionExhaustionAndRefills(t *testing.T) {
	now := int64(100) * hourMS
	samples := []Sample{
		{Provider: "codex", Account: "a", At: now - hourMS, Remaining: 60, ResetAt: ptr(now + 3*hourMS), PeriodHours: ptr(24.0)},
		{Provider: "codex", Account: "a", At: now, Remaining: 30, ResetAt: ptr(now + 3*hourMS), PeriodHours: ptr(24.0)},
		{Provider: "codex", Account: "b", At: now - hourMS, Remaining: 60, ResetAt: ptr(now + 5*hourMS), PeriodHours: ptr(24.0)},
		{Provider: "codex", Account: "b", At: now, Remaining: 40, ResetAt: ptr(now + 5*hourMS), PeriodHours: ptr(24.0)},
	}
	p := Pools(samples, now, 3, 1)[0].Projection
	if !p.Available || p.EmptyAt == nil || *p.EmptyAt != now+2*hourMS {
		t.Fatalf("wrong whole pool exhaustion: %+v", p)
	}
	if !reflect.DeepEqual(p.EmptyAccounts, []EmptyAccount{{"codex:a", now + hourMS}, {"codex:b", now + 2*hourMS}}) {
		t.Fatalf("empty accounts=%v", p.EmptyAccounts)
	}
	if p.Refills[0] != (Point{now + 3*hourMS, 100}) {
		t.Fatalf("refill=%v", p.Refills[0])
	}
	jump := false
	for i := 1; i < len(p.Points); i++ {
		if p.Points[i-1] == (Point{now + 3*hourMS, 0}) && p.Points[i] == (Point{now + 3*hourMS, 100}) {
			jump = true
		}
	}
	if !jump {
		t.Fatal("missing vertical refill jump")
	}
	for _, point := range p.Points {
		if point.Value < 0 || point.Value > 200.00001 {
			t.Fatalf("unbounded balance: %+v", point)
		}
	}
	p = Pools(samples, now, 3, 0)[0].Projection
	if !p.Available || p.EmptyAt != nil || len(p.EmptyAccounts) != 0 || p.Refills[0].Value != 70 {
		t.Fatalf("no-usage scenario=%+v", p)
	}
	p = Pools(samples, now, 3, 2)[0].Projection
	if p.EmptyAt == nil || *p.EmptyAt != now+hourMS {
		t.Fatal("multiplier not applied")
	}
	samples[3].ResetAt = ptr(now)
	if Pools(samples, now, 3, 1)[0].Projection.Available {
		t.Fatal("stale reset projected")
	}
}

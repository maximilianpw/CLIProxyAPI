package quotastats

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const hourMS = int64(time.Hour / time.Millisecond)

func validSample(s Sample) bool {
	return s.Provider != "" && s.Account != "" && s.At > 0 && !math.IsNaN(s.Remaining) && !math.IsInf(s.Remaining, 0) && s.Remaining >= 0 && s.Remaining <= 100 &&
		(s.ResetAt == nil || *s.ResetAt > 0) && (s.PeriodHours == nil || (!math.IsNaN(*s.PeriodHours) && !math.IsInf(*s.PeriodHours, 0) && *s.PeriodHours >= 1 && *s.PeriodHours <= 24*366))
}

// Normalize selects one window per account, never summing overlapping limits.
// Recognized longer windows win; otherwise use the furthest known reset.
// Unknown window periods stay unknown, so they cannot fabricate recurring refills.
func Normalize(provider, account string, at time.Time, response pluginapi.QuotaFetchResponse) (Sample, bool) {
	var selected Sample
	found := false
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			s := Sample{Provider: provider, Account: account, At: at.UnixMilli(), Remaining: bucket.RemainingFraction * 100, Window: group.DisplayName + "/" + bucket.Window}
			if reset, err := time.Parse(time.RFC3339Nano, bucket.ResetTime); err == nil {
				ms := reset.UnixMilli() - response.ServerTimeOffsetMs
				s.ResetAt = &ms
			}
			var period float64
			switch strings.ToLower(strings.TrimSpace(bucket.Window)) {
			case "weekly", "7d", "7-day", "7_day":
				period = 168
			case "daily", "24h", "1d":
				period = 24
			case "5h", "5-hour", "5_hour":
				period = 5
			case "hourly", "1h":
				period = 1
			}
			if period > 0 {
				s.PeriodHours = &period
			}
			if !validSample(s) {
				continue
			}
			oldPeriod := float64(0)
			if selected.PeriodHours != nil {
				oldPeriod = *selected.PeriodHours
			}
			if !found || period > oldPeriod || (period == oldPeriod && s.ResetAt != nil && (selected.ResetAt == nil || *s.ResetAt > *selected.ResetAt)) {
				selected, found = s, true
			}
		}
	}
	return selected, found
}

type Point struct {
	At    int64   `json:"at"`
	Value float64 `json:"value"`
}

type EmptyAccount struct {
	Name string `json:"name"`
	At   int64  `json:"at"`
}

type Projection struct {
	Available     bool           `json:"available"`
	Points        []Point        `json:"points"`
	Refills       []Point        `json:"refills"`
	EmptyAt       *int64         `json:"emptyAt"`
	EmptyAccounts []EmptyAccount `json:"emptyAccounts"`
}

type Account struct {
	Sample
	Name string   `json:"name"`
	Burn *float64 `json:"burn"`
}

type Pool struct {
	Provider     string     `json:"provider"`
	AccountCount int        `json:"accountCount"`
	Capacity     float64    `json:"capacity"`
	Remaining    float64    `json:"remaining"`
	Burn         *float64   `json:"burn"`
	Drain        *float64   `json:"drain"`
	Points       []Point    `json:"points"`
	Accounts     []Account  `json:"accounts"`
	Projection   Projection `json:"projection"`
}

func History(samples []Sample) map[string][]Sample {
	history := make(map[string][]Sample)
	for _, s := range samples {
		history[s.Name()] = append(history[s.Name()], s)
	}
	for _, series := range history {
		sort.Slice(series, func(i, j int) bool { return series[i].At < series[j].At })
	}
	return history
}

// Rate excludes refill/reset/window transitions and requires five minutes of evidence.
// A gap is not extrapolated across the left boundary of the lookback.
func Rate(samples []Sample, now int64, hours int) *float64 {
	var elapsed int64
	var consumed float64
	cutoff := now - int64(hours)*hourMS
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		if a.At < cutoff || b.At > now || b.At <= a.At || a.Window != b.Window || b.Window != samples[len(samples)-1].Window || b.Remaining > a.Remaining {
			continue
		}
		if (a.ResetAt == nil) != (b.ResetAt == nil) || (a.ResetAt != nil && *a.ResetAt != *b.ResetAt) {
			continue
		}
		// An interval crossing a reset cannot establish consumption, even if its timestamp is stale.
		if a.ResetAt != nil && *a.ResetAt > a.At && *a.ResetAt <= b.At {
			continue
		}
		elapsed += b.At - a.At
		consumed += a.Remaining - b.Remaining
	}
	if elapsed < int64(5*time.Minute/time.Millisecond) {
		return nil
	}
	rate := consumed / (float64(elapsed) / float64(hourMS))
	return &rate
}

func Pools(samples []Sample, now int64, hours int, multiplier float64) []Pool {
	history := History(samples)
	providers := make(map[string][]Account)
	for name, series := range history {
		last := series[len(series)-1]
		providers[last.Provider] = append(providers[last.Provider], Account{Sample: last, Name: name, Burn: Rate(series, now, hours)})
	}
	pools := make([]Pool, 0, len(providers))
	for provider, accounts := range providers {
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
		p := Pool{Provider: provider, AccountCount: len(accounts), Capacity: float64(len(accounts)) * 100, Accounts: accounts}
		burn, known := float64(0), true
		for _, a := range accounts {
			p.Remaining += a.Remaining
			if a.Burn == nil {
				known = false
			} else {
				burn += *a.Burn
			}
		}
		if known {
			p.Burn = &burn
			drain := burn / float64(len(accounts))
			p.Drain = &drain
		}
		p.Points = recorded(accounts, history)
		p.Projection = project(accounts, now, hours, multiplier)
		pools = append(pools, p)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Provider < pools[j].Provider })
	return pools
}

// recorded carries observations forward only within the common observed range.
func recorded(accounts []Account, history map[string][]Sample) []Point {
	start, end := int64(0), int64(math.MaxInt64)
	var events []Sample
	for _, a := range accounts {
		s := history[a.Name]
		start = max(start, s[0].At)
		end = min(end, s[len(s)-1].At)
		events = append(events, s...)
	}
	points := make([]Point, 0)
	if start > end {
		return points
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At < events[j].At })
	values := make(map[string]float64)
	var total float64
	for i := 0; i < len(events); {
		at := events[i].At
		if at > end {
			break
		}
		for i < len(events) && events[i].At == at {
			s := events[i]
			total += s.Remaining - values[s.Name()]
			values[s.Name()] = s.Remaining
			i++
		}
		if at >= start {
			points = append(points, Point{at, total})
		}
	}
	return points
}

func project(accounts []Account, now int64, hours int, multiplier float64) Projection {
	unavailable := Projection{Points: []Point{}, Refills: []Point{}, EmptyAccounts: []EmptyAccount{}}
	type event struct {
		at            int64
		refill, slope float64
		empty         string
	}
	var events []event
	end := now + 14*24*hourMS
	var total, slope float64
	for _, a := range accounts {
		if a.Burn == nil || a.ResetAt == nil || *a.ResetAt <= now || a.PeriodHours == nil || a.At < now-int64(hours)*hourMS {
			return unavailable
		}
		rate := *a.Burn * multiplier / float64(hourMS)
		total += a.Remaining
		if a.Remaining > 0 {
			slope -= rate
		}
		remaining, cursor, reset := a.Remaining, now, *a.ResetAt
		period := int64(*a.PeriodHours * float64(hourMS))
		emptyRecorded := false
		for cursor < end {
			next := min(reset, end)
			if remaining == 0 || (rate > 0 && remaining/rate <= float64(next-cursor)) {
				empty := cursor
				if remaining > 0 {
					empty += int64(math.Ceil(remaining / rate))
				}
				e := event{at: empty}
				if remaining > 0 {
					e.slope = rate
				}
				if !emptyRecorded {
					e.empty = a.Name
					emptyRecorded = true
				}
				events = append(events, e)
				remaining = 0
			} else {
				remaining -= rate * float64(next-cursor)
			}
			if next == reset {
				e := event{at: reset, refill: 100 - remaining}
				if remaining == 0 {
					e.slope = -rate
				}
				events = append(events, e)
				remaining = 100
				reset += period
			}
			cursor = next
			if len(events) > 100000 {
				return unavailable
			}
		}
	}
	p := unavailable
	p.Available = true
	p.Points = append(p.Points, Point{now, total})
	if total == 0 {
		at := now
		p.EmptyAt = &at
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	cursor := now
	for i := 0; i < len(events); {
		at := events[i].at
		total = math.Max(0, total+slope*float64(at-cursor))
		p.Points = append(p.Points, Point{at, total})
		if total < 1e-7 && p.EmptyAt == nil {
			empty := at
			p.EmptyAt = &empty
		}
		var refill float64
		for i < len(events) && events[i].at == at {
			e := events[i]
			refill += e.refill
			slope += e.slope
			if e.empty != "" {
				p.EmptyAccounts = append(p.EmptyAccounts, EmptyAccount{e.empty, at})
			}
			i++
		}
		if refill > 0 {
			total += refill
			p.Refills = append(p.Refills, Point{at, refill})
			p.Points = append(p.Points, Point{at, total})
		}
		cursor = at
	}
	if cursor < end {
		p.Points = append(p.Points, Point{end, math.Max(0, total+slope*float64(end-cursor))})
	}
	return p
}

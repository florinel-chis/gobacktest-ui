package main

import (
	"context"
	"math"
	"testing"
	"time"

	backtest "github.com/florinel-chis/gobacktest"
	"github.com/florinel-chis/gobacktest/source"
)

// weekdayBars returns one bar per weekday (Mon–Fri) from start, skipping the
// given holidays; the close rises by 1 each session, starting at 10.
func weekdayBars(start time.Time, sessions int, holidays ...string) []backtest.Bar {
	skip := map[string]bool{}
	for _, h := range holidays {
		skip[h] = true
	}
	var bars []backtest.Bar
	px := 10.0
	for d := start; len(bars) < sessions; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || skip[d.Format("2006-01-02")] {
			continue
		}
		bars = append(bars, backtest.Bar{Time: d, Open: px, High: px + 0.5, Low: px - 0.5, Close: px, Volume: 1000})
		px++
	}
	return bars
}

func runDCA(t *testing.T, bars []backtest.Bar, weekday time.Weekday, units float64) *backtest.Result {
	t.Helper()
	res, err := backtest.New(backtest.FromBars(bars), &dcaStrat{weekday: weekday, units: units},
		dcaOptions(backtest.Options{Cash: 100_000, Margin: 1, FinalizeTrades: true})).Run()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func entryPrices(res *backtest.Result) []float64 {
	var out []float64
	for _, tr := range res.Trades {
		out = append(out, tr.EntryPrice)
	}
	return out
}

// The assertions below check fill PRICES: each buy fills at the close of the
// day it was placed. (Under TradeOnClose the published engine stamps
// Trade.EntryTime with the following bar; backtesting.py stamps the placing
// bar. Assert dates once the engine fix is released.)

func TestDCABuysEveryTargetWeekdayAtThatClose(t *testing.T) {
	// Mon 2026-01-05 + 16 sessions (ends Mon 2026-01-26): Fridays 9, 16, 23 Jan
	// close at 14, 19, 24.
	bars := weekdayBars(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 16)
	got := entryPrices(runDCA(t, bars, time.Friday, 1))
	if want := []float64{14, 19, 24}; !equalFloats(got, want) {
		t.Fatalf("fills at %v, want each Friday's close %v", got, want)
	}
}

func TestDCAHolidayBuysNextSession(t *testing.T) {
	// Friday 2026-01-16 is closed: that week's buy is Monday 2026-01-19 (close
	// 19); Friday 2026-01-23 closes at 23.
	bars := weekdayBars(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 16, "2026-01-16")
	got := entryPrices(runDCA(t, bars, time.Friday, 1))
	if want := []float64{14, 19, 23}; !equalFloats(got, want) {
		t.Fatalf("fills at %v, want %v", got, want)
	}
}

func TestDCALastBarOrderDoesNotFill(t *testing.T) {
	// 15 sessions end on Friday 2026-01-23: that order has no later bar to fill on.
	bars := weekdayBars(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 15)
	if got := entryPrices(runDCA(t, bars, time.Friday, 1)); !equalFloats(got, []float64{14, 19}) {
		t.Fatalf("fills at %v, want [14 19]", got)
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDCAUnitsPerBuy(t *testing.T) {
	bars := weekdayBars(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 10)
	res := runDCA(t, bars, time.Wednesday, 3)
	if len(res.Trades) != 2 {
		t.Fatalf("trades = %d, want 2", len(res.Trades))
	}
	for _, tr := range res.Trades {
		if tr.Size != 3 {
			t.Errorf("size = %v, want 3", tr.Size)
		}
	}
}

func TestSummarizeDCA(t *testing.T) {
	trades := []backtest.Trade{{Size: 1, EntryPrice: 10}, {Size: 2, EntryPrice: 13}}
	s := summarizeDCA(trades, 15)
	if s.Buys != 2 || s.Units != 3 || s.Invested != 36 || s.AvgCost != 12 || s.Value != 45 || s.PL != 9 {
		t.Fatalf("summary = %+v", s)
	}
	if math.Abs(s.PLPct-25) > 1e-9 {
		t.Errorf("PLPct = %v, want 25", s.PLPct)
	}
}

func TestBuildStrategyDCA(t *testing.T) {
	if _, err := buildStrategy(runReq{Strategy: "dca", DCAWeekday: 5, DCAUnits: 1}); err != nil {
		t.Fatalf("valid dca rejected: %v", err)
	}
	for _, bad := range []runReq{
		{Strategy: "dca", DCAWeekday: 0, DCAUnits: 1},
		{Strategy: "dca", DCAWeekday: 6, DCAUnits: 1},
		{Strategy: "dca", DCAWeekday: 5, DCAUnits: 0},
		{Strategy: "dca", DCAWeekday: 5, DCAUnits: 1.5},
	} {
		if _, err := buildStrategy(bad); err == nil {
			t.Errorf("want error for %+v", bad)
		}
	}
}

func TestNewSourceBVB(t *testing.T) {
	src, err := newSource(runReq{Source: "bvb"})
	if err != nil || src == nil {
		t.Fatalf("bvb source: %v", err)
	}
}

// fakeSource serves fixed bars, so runBacktest can be exercised offline.
type fakeSource struct{ bars []backtest.Bar }

func (f fakeSource) Fetch(_ context.Context, _ string, _, _ time.Time, _ source.Interval) (*backtest.Data, error) {
	return backtest.FromBars(f.bars), nil
}

func TestRunBacktestDCA(t *testing.T) {
	// 41 sessions: 8 full weeks plus Monday, so the 8th Friday's order (placed
	// on that bar, filled at its close while the next bar is processed) fills.
	bars := weekdayBars(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 41)
	orig := sourceFor
	sourceFor = func(runReq) (source.Source, error) { return fakeSource{bars}, nil }
	t.Cleanup(func() { sourceFor = orig })

	// No take-profit needed for DCA.
	resp := runBacktest(runReq{Source: "bvb", Symbol: "TLV", Strategy: "dca", DCAWeekday: 5, DCAUnits: 1,
		Start: "2026-01-05", End: "2026-03-31"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	rows := map[string]statRow{}
	for _, r := range resp.Stats {
		rows[r.Label] = r
	}
	// 8 Friday buys of 1 unit at closes 14, 19, …, 49; last close 50.
	want := map[string]string{"Buys": "8", "Units held": "8", "Cash invested": "252.00", "Average cost": "31.50",
		"Market value": "400.00", "P/L": "+148.00 (+58.73%)"}
	for label, v := range want {
		if rows[label].Value != v {
			t.Errorf("%s = %q, want %q", label, rows[label].Value, v)
		}
	}
	// DCA never sells: the engine's end-of-data finalisation is not a trade to draw.
	for _, m := range resp.Markers {
		if m.Kind == "sell" {
			t.Fatalf("unexpected sell marker at %v", m.Time)
		}
	}
	if resp.Stats[0].Label != "Buys" {
		t.Errorf("DCA rows should come first, got %q", resp.Stats[0].Label)
	}
}

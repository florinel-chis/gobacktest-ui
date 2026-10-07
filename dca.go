package main

import (
	"time"

	backtest "github.com/florinel-chis/gobacktest"
)

// dcaStrat buys a fixed number of units on a weekly schedule and never sells
// (dollar-cost averaging). It keeps the next due date: on the first bar dated
// on or after it, it buys and moves the date to the following target weekday.
// A holiday on the target weekday therefore buys on the next session, and the
// schedule never looks at future bars. Run it with dcaOptions so each buy
// fills at the close of the bar it was placed on.
type dcaStrat struct {
	weekday time.Weekday
	units   float64
	due     time.Time
}

func (s *dcaStrat) Init(*backtest.State) {}

func (s *dcaStrat) Next(st *backtest.State) {
	times := st.Data().Time()
	day := dateOnly(times[len(times)-1])
	if s.due.IsZero() {
		s.due = nextWeekday(day, s.weekday)
	}
	if day.Before(s.due) {
		return
	}
	st.Buy(backtest.Order{Size: s.units})
	s.due = nextWeekday(day.AddDate(0, 0, 1), s.weekday)
}

// dcaOptions adapts run options for scheduled buying: market orders fill at
// the close of the bar that placed them ("buy on Friday" = Friday's close).
// An order placed on the very last bar has no later bar to fill on.
func dcaOptions(o backtest.Options) backtest.Options {
	o.TradeOnClose = true
	return o
}

// dateOnly truncates t to its UTC calendar date.
func dateOnly(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// nextWeekday returns the first date on or after from that falls on wd.
func nextWeekday(from time.Time, wd time.Weekday) time.Time {
	return from.AddDate(0, 0, (int(wd)-int(from.Weekday())+7)%7)
}

// dcaSummary is what a scheduled-buying run amounts to at the last close.
type dcaSummary struct {
	Buys      int
	Units     float64
	Invested  float64 // cash paid for the fills (spread included in the fill price)
	AvgCost   float64
	LastClose float64
	Value     float64
	PL        float64
	PLPct     float64
}

// summarizeDCA totals the entries of a DCA run and values the holding at
// lastClose. Exits are ignored: the engine's end-of-data finalisation closes
// open trades, but DCA holds them, so value comes from the last close.
func summarizeDCA(trades []backtest.Trade, lastClose float64) dcaSummary {
	s := dcaSummary{LastClose: lastClose}
	for _, t := range trades {
		s.Buys++
		s.Units += t.Size
		s.Invested += t.Size * t.EntryPrice
	}
	if s.Units > 0 {
		s.AvgCost = s.Invested / s.Units
	}
	s.Value = s.Units * lastClose
	s.PL = s.Value - s.Invested
	if s.Invested > 0 {
		s.PLPct = s.PL / s.Invested * 100
	}
	return s
}

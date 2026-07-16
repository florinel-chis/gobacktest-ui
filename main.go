// Command gobacktest-ui serves an interactive multi-source backtesting web
// lab. Pick a data source (Yahoo, Oanda, or Trading 212), a symbol/instrument,
// a bar interval, a date range, and a strategy — either the custom condition
// builder (AND/OR indicator conditions, take-profit / stop-loss / time exit,
// one-at-a-time or pyramiding) or a library strategy from the strategies
// package (Williams %R oversold, with or without the EMA filter) — run the
// backtest against live data, and see the equity curve vs buy & hold, the
// price with trade markers, and a full stats scorecard.
//
// The Oanda source needs OANDA_TOKEN on the server (env or .env); the
// Trading 212 source needs T212_API_KEY / T212_API_SECRET (env, .env.t212,
// or .env — the api_key_id / secret key names of a .env.t212 file also
// work). Credentials never reach the browser. Trading 212 has no candle
// endpoints: its tickers (AAPL_US_EQ, TMGl_EQ, ...) are resolved to Yahoo
// symbols by the trading212-go backtestsource adapter and the bars come
// from Yahoo, which allows intraday intervals down to 1m (with Yahoo's
// lookback limits: 1m ~7 days, 5m-30m ~60 days, 1h ~730 days).
//
// stdlib net/http only; Lightweight Charts is vendored and served locally.
//
//	go run .                     # then open http://localhost:8080
//	go run . -addr :9000
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	backtest "github.com/florinel-chis/gobacktest"
	"github.com/florinel-chis/gobacktest-ui/internal/dotenv"
	"github.com/florinel-chis/gobacktest/costs"
	"github.com/florinel-chis/gobacktest/indicators"
	"github.com/florinel-chis/gobacktest/report"
	"github.com/florinel-chis/gobacktest/source"
	"github.com/florinel-chis/gobacktest/strategies"
	oanda "github.com/florinel-chis/oanda-go"
	oandabs "github.com/florinel-chis/oanda-go/backtestsource"
	t212 "github.com/florinel-chis/trading212-go"
	t212bs "github.com/florinel-chis/trading212-go/backtestsource"
	yahoobs "github.com/florinel-chis/yahoo-go/backtestsource"
)

// envFiles are searched for OANDA_TOKEN when the Oanda source is selected.
var envFiles = []string{".env"}

// t212EnvFiles are searched for the Trading 212 key pair.
var t212EnvFiles = []string{".env.t212", ".env"}

//go:embed index.html
var indexHTML []byte

// Served at /lwc.js; reuses the vendored bundle embedded in the report package.
var lwcJS = report.LightweightChartsJS()

// ---- request / response types --------------------------------------------

type condReq struct {
	Indicator string  `json:"indicator"`
	P1        int     `json:"p1"`
	P2        int     `json:"p2"`
	Op        string  `json:"op"`
	Value     float64 `json:"value"`
}

type runReq struct {
	Source     string    `json:"source"`   // "yahoo" (default) | "oanda"
	Interval   string    `json:"interval"` // canonical source.Interval; "" -> 1d
	Symbol     string    `json:"symbol"`
	Start      string    `json:"start"`
	End        string    `json:"end"`
	Cash       float64   `json:"cash"`
	Strategy   string    `json:"strategy"` // "conditions" (default) | "wr" | "wrema"
	Conditions []condReq `json:"conditions"`
	Logic      string    `json:"logic"` // "AND" | "OR"
	TP         float64   `json:"tp"`    // percent, required > 0
	SL         float64   `json:"sl"`    // percent, 0 = none (conditions strategy only)
	TimeExit   int       `json:"timeExit"`
	Pyramid    bool      `json:"pyramid"`
	LotPct     float64   `json:"lotPct"`   // percent of buying power per lot (pyramid)
	Cooldown   int       `json:"cooldown"` // bars between entries
	WRPeriod   int       `json:"wrPeriod"` // wr/wrema strategies; 0 -> 21
	EMAPeriod  int       `json:"emaPeriod"`
	WRThr      float64   `json:"wrThr"` // 0 -> -80
	EMAThr     float64   `json:"emaThr"`
	SpreadPts  float64   `json:"spreadPts"`  // full bid-ask width in price points; half applied per fill
	FinRatePct float64   `json:"finRatePct"` // financing cost, percent of notional per year (long)
	Leverage   float64   `json:"leverage"`   // 0 or 1 = unleveraged; N -> Options.Margin = 1/N
}

// vp.Time / marker.Time are chart times: a "YYYY-MM-DD" string for daily and
// coarser bars, a UNIX-seconds number for intraday (both accepted by
// Lightweight Charts).
type vp struct {
	Time  any     `json:"time"`
	Value float64 `json:"value"`
}

type marker struct {
	Time  any     `json:"time"`
	Kind  string  `json:"kind"` // "buy" | "sell"
	Price float64 `json:"price"`
	PL    float64 `json:"pl"`
}

type statRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	BH    string `json:"bh"`
}

type runResp struct {
	Error   string    `json:"error,omitempty"`
	Warning string    `json:"warning,omitempty"`
	Symbol  string    `json:"symbol"`
	Range   string    `json:"range"`
	Equity  []vp      `json:"equity"`
	BuyHold []vp      `json:"buyhold"`
	Price   []vp      `json:"price"`
	Markers []marker  `json:"markers"`
	Stats   []statRow `json:"stats"`
}

// ---- generic condition-driven strategy -----------------------------------

type liveCond struct {
	req condReq
	ind *backtest.Indicator
}

func (c *liveCond) met() bool {
	v := c.ind.Last()
	if math.IsNaN(v) {
		return false
	}
	switch c.req.Op {
	case "<":
		return v < c.req.Value
	case "<=":
		return v <= c.req.Value
	case ">":
		return v > c.req.Value
	case ">=":
		return v >= c.req.Value
	}
	return false
}

type genStrat struct {
	req   runReq
	conds []*liveCond

	lastEntry int
	entryBar  int
}

func (s *genStrat) Init(st *backtest.State) {
	h, l, c := st.Data().High(), st.Data().Low(), st.Data().Close()
	s.lastEntry = -1 << 30
	for i, cr := range s.req.Conditions {
		series := indicatorSeries(cr, h, l, c)
		name := fmt.Sprintf("cond%d", i)
		s.conds = append(s.conds, &liveCond{req: cr, ind: st.I(name, func() []float64 { return series })})
	}
}

func (s *genStrat) signal() bool {
	if len(s.conds) == 0 {
		return false
	}
	if s.req.Logic == "OR" {
		for _, c := range s.conds {
			if c.met() {
				return true
			}
		}
		return false
	}
	for _, c := range s.conds { // AND (default)
		if !c.met() {
			return false
		}
	}
	return true
}

func (s *genStrat) Next(st *backtest.State) {
	cur := st.Data().Len() - 1
	tp := s.req.TP / 100
	sl := s.req.SL / 100

	for _, t := range st.OpenTrades() {
		if t.TP() == 0 {
			t.SetTP(t.EntryPrice() * (1 + tp))
			if sl > 0 {
				t.SetSL(t.EntryPrice() * (1 - sl))
			}
			if !s.req.Pyramid {
				s.entryBar = cur
			}
		}
	}

	// Time exit (single-position mode only; per-lot ages aren't tracked in pyramid).
	if !s.req.Pyramid && s.req.TimeExit > 0 && st.Position().Size() != 0 && cur-s.entryBar >= s.req.TimeExit {
		st.Position().Close()
		return
	}

	if len(st.Orders()) != 0 {
		return
	}
	eligible := s.req.Pyramid && cur-s.lastEntry >= s.req.Cooldown
	if !s.req.Pyramid {
		eligible = st.Position().Size() == 0
	}
	if eligible && s.signal() {
		size := entrySize(s.req.Pyramid, s.req.LotPct)
		if size <= 0 {
			return
		}
		st.Buy(backtest.Order{Size: size})
		s.lastEntry = cur
	}
}

// entrySize returns the order size for a new entry as a fraction of buying
// power. The engine treats |size| >= 1 as absolute units (shares), so the
// fraction is capped at 0.9999: pyramid LotPct=100 means all-in, matching the
// single-position semantics, rather than silently buying exactly one share.
func entrySize(pyramid bool, lotPct float64) float64 {
	if !pyramid {
		return 0.9999
	}
	return math.Min(lotPct/100, 0.9999)
}

// indicatorSeries returns the full-length (NaN-warmup) series a condition reads.
func indicatorSeries(c condReq, h, l, cl backtest.Series) []float64 {
	p := c.P1
	if p <= 0 {
		p = 14
	}
	switch c.Indicator {
	case "rsi":
		return indicators.RSI(cl, p)
	case "wr":
		return indicators.WilliamsR(h, l, cl, p)
	case "wrema":
		e := c.P2
		if e <= 0 {
			e = 13
		}
		return indicators.EMA(indicators.WilliamsR(h, l, cl, p), e)
	case "cci":
		return indicators.CCI(h, l, cl, p)
	case "stoch_k":
		k, _ := indicators.Stochastic(h, l, cl, p, 3, 3)
		return k
	case "roc":
		return indicators.ROC(cl, p)
	case "adx":
		return indicators.ADX(h, l, cl, p)
	case "bb_pctb": // %B: (close-lower)/(upper-lower); <0 below lower band, >1 above upper
		upper, _, lower := indicators.Bollinger(cl, p, 2.0)
		out := make([]float64, len(cl))
		for i := range out {
			rng := upper[i] - lower[i]
			if math.IsNaN(rng) || rng == 0 {
				out[i] = math.NaN()
			} else {
				out[i] = (cl[i] - lower[i]) / rng
			}
		}
		return out
	case "sma_dist": // percent distance of close from SMA(p); <0 = below SMA
		sma := indicators.SMA(cl, p)
		out := make([]float64, len(cl))
		for i := range out {
			if math.IsNaN(sma[i]) || sma[i] == 0 {
				out[i] = math.NaN()
			} else {
				out[i] = (cl[i]/sma[i] - 1) * 100
			}
		}
		return out
	default:
		// Unknown indicator: all-NaN series never triggers.
		out := make([]float64, len(cl))
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
}

// buyHold buys all-in on the first bar and holds.
type buyHold struct{}

func (buyHold) Init(*backtest.State) {}
func (buyHold) Next(st *backtest.State) {
	if st.Position().Size() == 0 && len(st.Orders()) == 0 {
		st.Buy(backtest.Order{Size: 0.9999})
	}
}

// ---- backtest execution --------------------------------------------------

// newSource resolves the requested data source. Credentials come from the
// environment / .env files and are never sent to or read from the browser.
func newSource(req runReq) (source.Source, error) {
	switch req.Source {
	case "", "yahoo":
		return yahoobs.New(), nil
	case "oanda":
		tok := dotenv.Get("OANDA_TOKEN", envFiles...)
		if tok == "" {
			return nil, fmt.Errorf("oanda source: OANDA_TOKEN not set on the server (env or %s)", strings.Join(envFiles, ", "))
		}
		return oandabs.New(oanda.New(tok)), nil
	case "t212":
		return t212Source()
	}
	return nil, fmt.Errorf("unknown source %q (want yahoo, oanda or t212)", req.Source)
}

// t212Cache holds the process-wide Trading 212 source. The adapter caches the
// ~17k-instrument list for its lifetime and the instruments endpoint allows
// one request per 50 seconds, so building a fresh adapter per backtest run
// would rate-limit the second run — the source must be shared.
var t212Cache struct {
	sync.Mutex
	src source.Source
}

// t212Source returns the shared Trading 212 source, building it on first use
// from T212_API_KEY / T212_API_SECRET (or the api_key_id / secret names used
// by a .env.t212 file). Errors are not cached so a fixed environment is
// picked up on the next request.
func t212Source() (source.Source, error) {
	t212Cache.Lock()
	defer t212Cache.Unlock()
	if t212Cache.src != nil {
		return t212Cache.src, nil
	}
	key := dotenv.Get("T212_API_KEY", t212EnvFiles...)
	if key == "" {
		key = dotenv.Get("api_key_id", t212EnvFiles...)
	}
	secret := dotenv.Get("T212_API_SECRET", t212EnvFiles...)
	if secret == "" {
		secret = dotenv.Get("secret", t212EnvFiles...)
	}
	if key == "" || secret == "" {
		return nil, fmt.Errorf("t212 source: T212_API_KEY / T212_API_SECRET not set on the server (env or %s)", strings.Join(t212EnvFiles, ", "))
	}
	t212Cache.src = t212bs.New(t212.New(key, secret), yahoobs.New())
	return t212Cache.src, nil
}

// buildStrategy resolves the requested strategy. "conditions" is the custom
// condition builder; "wr"/"wrema" are the library strategies. The canned
// strategies manage their own exit (take-profit only), so SL / time exit /
// pyramiding inputs do not apply to them.
func buildStrategy(req runReq) (backtest.Strategy, error) {
	switch req.Strategy {
	case "", "conditions":
		if err := validateConditions(req.Conditions); err != nil {
			return nil, err
		}
		if err := validateLot(req.Pyramid, req.LotPct); err != nil {
			return nil, err
		}
		return &genStrat{req: req}, nil
	case "wr", "wrema":
		if req.WRPeriod < 0 || req.WRPeriod > 1000 || req.EMAPeriod < 0 || req.EMAPeriod > 1000 {
			return nil, fmt.Errorf("wr/ema periods must be in 0..1000")
		}
		return &strategies.WilliamsROversold{
			WRPeriod:     req.WRPeriod,
			EMAPeriod:    req.EMAPeriod,
			WRThreshold:  req.WRThr,
			EMAThreshold: req.EMAThr,
			UseEMA:       req.Strategy == "wrema",
			TPPct:        req.TP,
			Size:         0.9999, // all-in per entry, matching the condition strategy's sizing
		}, nil
	}
	return nil, fmt.Errorf("unknown strategy %q (want conditions, wr or wrema)", req.Strategy)
}

// chartTime renders a bar time for Lightweight Charts: date string for daily
// and coarser intervals, UNIX seconds for intraday.
func chartTime(t time.Time, intraday bool) any {
	if intraday {
		return t.Unix()
	}
	return t.Format("2006-01-02")
}

// intradayInterval reports whether the canonical interval is finer than daily.
func intradayInterval(iv string) bool {
	switch source.Interval(iv) {
	case "", source.D1, source.W1, source.Mo1:
		return false
	}
	return true
}

func runBacktest(req runReq) runResp {
	if req.Symbol == "" {
		req.Symbol = "SPY"
	}
	if req.Cash <= 0 {
		req.Cash = 10_000
	}
	if req.TP <= 0 {
		return runResp{Error: "take-profit (tp) must be greater than 0"}
	}
	if req.SpreadPts < 0 {
		return runResp{Error: "spread must be >= 0"}
	}
	if req.FinRatePct < -50 || req.FinRatePct > 50 {
		return runResp{Error: "financing rate must be between -50 and 50 percent per year"}
	}
	if req.Leverage == 0 {
		req.Leverage = 1
	}
	if req.Leverage < 1 || req.Leverage > 100 {
		return runResp{Error: "leverage must be between 1 and 100"}
	}
	strat, err := buildStrategy(req)
	if err != nil {
		return runResp{Error: err.Error()}
	}
	src, err := newSource(req)
	if err != nil {
		return runResp{Error: err.Error()}
	}
	interval := source.Interval(req.Interval)
	if interval == "" {
		interval = source.D1
	}
	end := parseDate(req.End, time.Now())
	start := parseDate(req.Start, end.AddDate(-5, 0, 0))
	if !start.Before(end) {
		return runResp{Error: "start date must be before end date"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	data, err := src.Fetch(ctx, req.Symbol, start, end, interval)
	if err != nil {
		return runResp{Error: fmt.Sprintf("fetch %s: %v", req.Symbol, err)}
	}
	bars := data.Bars()
	if len(bars) < 30 {
		return runResp{Error: fmt.Sprintf("only %d bars for %s — widen the date range", len(bars), req.Symbol)}
	}

	// The engine's Spread is a per-fill price fraction applied in the adverse
	// direction; candles are mid, so each fill crosses half the bid-ask width.
	spreadFrac := 0.0
	if req.SpreadPts > 0 {
		spreadFrac = req.SpreadPts / 2 / bars[0].Close
	}
	opts := backtest.Options{
		Cash:           req.Cash,
		Margin:         1 / req.Leverage,
		Spread:         spreadFrac,
		FinalizeTrades: false,
	}

	stRes, err := backtest.New(backtest.FromBars(bars), strat, opts).Run()
	if err != nil {
		return runResp{Error: fmt.Sprintf("run: %v", err)}
	}
	stStats := backtest.Compute(stRes, backtest.FromBars(bars), 0)

	// Buy & hold: finalize so its single never-closing position counts toward
	// exposure (~100%) and trade stats rather than reading as 0. It stays
	// UNLEVERAGED on purpose — it is the "just hold the asset" benchmark —
	// but pays the same spread and financing as any real position.
	bhOpts := opts
	bhOpts.FinalizeTrades = true
	bhOpts.Margin = 1
	bhRes, _ := backtest.New(backtest.FromBars(bars), buyHold{}, bhOpts).Run()
	bhStats := backtest.Compute(bhRes, backtest.FromBars(bars), 0)

	finRate := req.FinRatePct / 100
	finStrat := costs.FinancingPct(stRes.Trades, finRate, req.Cash)
	finBH := costs.FinancingPct(bhRes.Trades, finRate, req.Cash)

	intraday := intradayInterval(req.Interval)
	resp := runResp{
		Symbol: req.Symbol,
		Range:  fmt.Sprintf("%s → %s (%d bars)", bars[0].Time.Format("2006-01-02"), bars[len(bars)-1].Time.Format("2006-01-02"), len(bars)),
	}
	for _, p := range stRes.EquityCurve {
		resp.Equity = append(resp.Equity, vp{chartTime(p.Time, intraday), jnum(p.Equity)})
	}
	for _, p := range bhRes.EquityCurve {
		resp.BuyHold = append(resp.BuyHold, vp{chartTime(p.Time, intraday), jnum(p.Equity)})
	}
	for _, b := range bars {
		resp.Price = append(resp.Price, vp{chartTime(b.Time, intraday), jnum(b.Close)})
	}
	for _, t := range stRes.Trades {
		resp.Markers = append(resp.Markers, marker{chartTime(t.EntryTime, intraday), "buy", jnum(t.EntryPrice), 0})
		if !t.ExitTime.IsZero() {
			resp.Markers = append(resp.Markers, marker{chartTime(t.ExitTime, intraday), "sell", jnum(t.ExitPrice), jnum(t.PL)})
		}
	}

	tradesPerMo := 0.0
	if mo := bars[len(bars)-1].Time.Sub(bars[0].Time).Hours() / 24 / 30.44; mo > 0 {
		tradesPerMo = float64(stStats.NumTrades) / mo
	}
	resp.Stats = []statRow{
		{"Return", pct(stStats.ReturnPct), pct(bhStats.ReturnPct)},
		{"Financing cost", pct(finStrat), pct(finBH)},
		{"Net Return", pct(stStats.ReturnPct - finStrat), pct(bhStats.ReturnPct - finBH)},
		{"Sharpe", f2(stStats.SharpeRatio), f2(bhStats.SharpeRatio)},
		{"Max Drawdown", pct(stStats.MaxDrawdownPct), pct(bhStats.MaxDrawdownPct)},
		{"CAGR", pct(stStats.CAGRPct), pct(bhStats.CAGRPct)},
		{"Exposure", pct(stStats.ExposureTimePct), pct(bhStats.ExposureTimePct)},
		{"# Trades", fmt.Sprintf("%d", stStats.NumTrades), "1"},
		{"Trades / month", f2(tradesPerMo), "—"},
		{"Win Rate", pct(stStats.WinRatePct), "—"},
		{"Profit Factor", profitFactor(stStats), "—"},
		{"Final Equity", money(stStats.EquityFinal), money(bhStats.EquityFinal)},
	}
	return resp
}

func parseDate(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return def
	}
	return t
}

// profitFactor shows infinity when there are trades but no losers (PF is
// gross-profit / gross-loss, NaN at zero losses), else the numeric value.
func profitFactor(s backtest.Stats) string {
	if s.NumTrades > 0 && math.IsNaN(s.ProfitFactor) && s.WinRatePct >= 100 {
		return "∞"
	}
	return f2(s.ProfitFactor)
}

// jnum returns a JSON-safe rounded value. NaN/Inf would make json.Encode fail
// mid-write (a truncated response that the browser reports as "Failed to fetch"),
// so they are coerced to 0.
func jnum(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*100) / 100
}

// knownIndicators mirrors the indicatorSeries switch — kept in sync so an unknown
// indicator is rejected up front rather than silently producing an all-NaN series.
var knownIndicators = map[string]bool{
	"rsi": true, "wr": true, "wrema": true, "cci": true, "stoch_k": true,
	"roc": true, "adx": true, "bb_pctb": true, "sma_dist": true,
}
var knownOps = map[string]bool{"<": true, "<=": true, ">": true, ">=": true}

// validateLot rejects pyramid lot percentages the engine cannot express as a
// fraction of buying power (see entrySize).
func validateLot(pyramid bool, lotPct float64) error {
	if pyramid && (lotPct <= 0 || lotPct > 100) {
		return fmt.Errorf("lot %% (lotPct) must be greater than 0 and at most 100 when pyramiding, got %g", lotPct)
	}
	return nil
}

func validateConditions(cs []condReq) error {
	if len(cs) == 0 {
		return fmt.Errorf("add at least one entry condition")
	}
	for i, c := range cs {
		if !knownIndicators[c.Indicator] {
			return fmt.Errorf("condition %d: unknown indicator %q", i+1, c.Indicator)
		}
		if !knownOps[c.Op] {
			return fmt.Errorf("condition %d: invalid operator %q", i+1, c.Op)
		}
		if c.P1 < 0 || c.P1 > 1000 || c.P2 < 0 || c.P2 > 1000 {
			return fmt.Errorf("condition %d: period out of range (0–1000)", i+1)
		}
	}
	return nil
}
func f2(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "—"
	}
	return fmt.Sprintf("%.2f", f)
}
func pct(f float64) string   { return f2(f) + "%" }
func money(f float64) string { return fmt.Sprintf("$%.0f", f) }

// ---- HTTP ----------------------------------------------------------------

func main() {
	// Loopback by default: the app has no auth, so it must not listen on all
	// interfaces unless explicitly asked to (e.g. -addr :8080).
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/lwc.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write(lwcJS)
	})
	mux.HandleFunc("/api/backtest", recoverJSON(handleBacktest))
	mux.HandleFunc("/api/instrument", recoverJSON(handleInstrument))

	log.Printf("gobacktest web UI on %s", listenURL(*addr))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second, // must exceed the 30s Yahoo fetch in the handler
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// listenURL renders a listen address as a browsable URL for the startup log.
func listenURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	return "http://" + addr
}

// handleBacktest decodes the request, runs the backtest, and logs the outcome.
func handleBacktest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	t0 := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // requests are tiny JSON; cap at 1 MiB
	var req runReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("backtest: bad request body: %v", err)
		writeJSON(w, runResp{Error: "bad request: " + err.Error()})
		return
	}
	resp := runBacktest(req)
	dur := time.Since(t0).Round(time.Millisecond)
	strat := req.Strategy
	if strat == "" {
		strat = "conditions"
	}
	if resp.Error != "" {
		log.Printf("backtest FAIL src=%s iv=%s sym=%s strat=%s conds=%s pyramid=%v tp=%.1f sl=%.1f -> %q (%s)",
			req.Source, req.Interval, req.Symbol, strat, condSummary(req.Conditions), req.Pyramid, req.TP, req.SL, resp.Error, dur)
	} else {
		log.Printf("backtest OK   src=%s iv=%s sym=%s strat=%s conds=%s pyramid=%v tp=%.1f sl=%.1f -> %d markers, %d eq pts (%s)",
			req.Source, req.Interval, req.Symbol, strat, condSummary(req.Conditions), req.Pyramid, req.TP, req.SL, len(resp.Markers), len(resp.Equity), dur)
	}
	writeJSON(w, resp)
}

// instrumentResp is the /api/instrument payload: live cost facts used to
// prefill the "Costs & leverage" section. FinRatePct is the LONG financing
// cost as a positive percent per year (Oanda reports costs as negative rates).
type instrumentResp struct {
	Error      string  `json:"error,omitempty"`
	SpreadPts  float64 `json:"spreadPts"`
	FinRatePct float64 `json:"finRatePct"`
	Precision  int     `json:"precision"`
	MarginRate float64 `json:"marginRate"`
}

// handleInstrument serves live spread/financing facts for an Oanda instrument
// (GET /api/instrument?instrument=EU50_EUR). Yahoo has no cost API.
func handleInstrument(w http.ResponseWriter, r *http.Request) {
	instrument := r.URL.Query().Get("instrument")
	if instrument == "" {
		writeJSON(w, instrumentResp{Error: "instrument query parameter required"})
		return
	}
	tok := dotenv.Get("OANDA_TOKEN", envFiles...)
	if tok == "" {
		writeJSON(w, instrumentResp{Error: "OANDA_TOKEN not set on the server — auto-fill needs the Oanda source configured"})
		return
	}
	client := oanda.New(tok)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	facts, err := client.InstrumentFacts(ctx, instrument)
	if err != nil {
		writeJSON(w, instrumentResp{Error: fmt.Sprintf("instrument facts: %v", err)})
		return
	}
	spread, err := client.AvgSpread(ctx, instrument, 50)
	if err != nil {
		writeJSON(w, instrumentResp{Error: fmt.Sprintf("spread: %v", err)})
		return
	}
	log.Printf("instrument facts %s: spread=%.3f finLong=%.2f%%/yr margin=%.2f", instrument, spread, -facts.LongRate*100, facts.MarginRate)
	writeJSON(w, instrumentResp{
		SpreadPts:  math.Round(spread*1000) / 1000,
		FinRatePct: math.Round(-facts.LongRate*10000) / 100, // cost as positive percent
		Precision:  facts.DisplayPrecision,
		MarginRate: facts.MarginRate,
	})
}

// condSummary renders conditions compactly for the log line.
func condSummary(cs []condReq) string {
	if len(cs) == 0 {
		return "(none)"
	}
	s := ""
	for i, c := range cs {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%s(%d/%d)%s%.1f", c.Indicator, c.P1, c.P2, c.Op, c.Value)
	}
	return s
}

// recoverJSON turns a handler panic — which otherwise drops the connection and
// shows in the browser as "TypeError: Failed to fetch" with no diagnostic — into
// a logged stack trace plus a JSON 500 the UI can display.
func recoverJSON(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC in %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(runResp{
					Error: fmt.Sprintf("server error: %v — see the terminal running the server for the stack trace", rec),
				})
			}
		}()
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Header/body already partly written; log so the truncated-response cause
		// of a client-side "Failed to fetch" is visible server-side.
		log.Printf("writeJSON encode error: %v", err)
	}
}

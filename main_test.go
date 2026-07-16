package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRecoverJSONTurnsPanicIntoJSON500 proves the middleware converts a handler
// panic (which would otherwise drop the connection → "Failed to fetch") into a
// JSON 500 the UI can display.
func TestRecoverJSONTurnsPanicIntoJSON500(t *testing.T) {
	h := recoverJSON(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/backtest", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp runResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !strings.Contains(resp.Error, "server error") || !strings.Contains(resp.Error, "boom") {
		t.Fatalf("error = %q, want it to mention the panic", resp.Error)
	}
}

func TestValidateConditions(t *testing.T) {
	cases := []struct {
		name    string
		conds   []condReq
		wantErr bool
	}{
		{"empty", nil, true},
		{"unknown indicator", []condReq{{Indicator: "xyz", Op: "<"}}, true},
		{"bad operator", []condReq{{Indicator: "rsi", Op: "=="}}, true},
		{"period too large", []condReq{{Indicator: "rsi", Op: "<", P1: 5000}}, true},
		{"valid bollinger", []condReq{{Indicator: "bb_pctb", Op: "<", P1: 20}}, false},
		{"valid wrema two params", []condReq{{Indicator: "wrema", Op: "<=", P1: 21, P2: 13}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConditions(tc.conds)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateConditions(%v) err = %v, wantErr = %v", tc.conds, err, tc.wantErr)
			}
		})
	}
}

// TestValidateLot proves pyramid lot sizing is validated server-side:
// LotPct must be > 0 and <= 100 when pyramiding (the engine treats
// |size| >= 1 as absolute units, so LotPct > 100 would buy shares, not percent).
func TestValidateLot(t *testing.T) {
	cases := []struct {
		name    string
		pyramid bool
		lotPct  float64
		wantErr bool
	}{
		{"pyramid zero", true, 0, true},
		{"pyramid negative", true, -5, true},
		{"pyramid over 100", true, 101, true},
		{"pyramid 100 all-in", true, 100, false},
		{"pyramid typical", true, 20, false},
		{"single-position ignores lotPct", false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLot(tc.pyramid, tc.lotPct)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateLot(%v, %v) err = %v, wantErr = %v", tc.pyramid, tc.lotPct, err, tc.wantErr)
			}
		})
	}
}

// TestEntrySize proves the order size stays a fraction of buying power:
// the engine reads |size| >= 1 as absolute units, so pyramid LotPct=100 must
// map to the 0.9999 all-in fraction, never to 1.0 (= exactly one share).
func TestEntrySize(t *testing.T) {
	cases := []struct {
		name    string
		pyramid bool
		lotPct  float64
		want    float64
	}{
		{"single-position all-in", false, 20, 0.9999},
		{"pyramid 20 percent", true, 20, 0.20},
		{"pyramid 50 percent", true, 50, 0.50},
		{"pyramid 100 capped below 1", true, 100, 0.9999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entrySize(tc.pyramid, tc.lotPct); got != tc.want {
				t.Fatalf("entrySize(%v, %v) = %v, want %v", tc.pyramid, tc.lotPct, got, tc.want)
			}
		})
	}
}

// TestHandleBacktestRejectsBadPyramidLot proves the handler returns the standard
// JSON error shape for an out-of-range pyramid lot (fails validation before any
// network fetch).
func TestHandleBacktestRejectsBadPyramidLot(t *testing.T) {
	for _, lotPct := range []float64{0, -5, 101} {
		body, _ := json.Marshal(runReq{
			Symbol:     "SPY",
			TP:         10,
			Conditions: []condReq{{Indicator: "rsi", Op: "<", P1: 14, Value: 30}},
			Pyramid:    true,
			LotPct:     lotPct,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(string(body)))
		handleBacktest(rec, req)

		var resp runResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("lotPct=%v: body is not JSON: %v (%q)", lotPct, err, rec.Body.String())
		}
		if resp.Error == "" || !strings.Contains(resp.Error, "lot") {
			t.Fatalf("lotPct=%v: error = %q, want a lot-percent validation error", lotPct, resp.Error)
		}
	}
}

// TestHandleBacktestRejectsOversizedBody proves request bodies over 1 MiB are
// rejected by the size limit rather than decoded. The oversized payload has
// tp=0 so even a regression cannot reach the network fetch.
func TestHandleBacktestRejectsOversizedBody(t *testing.T) {
	body := `{"symbol":"` + strings.Repeat("A", 2<<20) + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/backtest", strings.NewReader(body))
	handleBacktest(rec, req)

	var resp runResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !strings.Contains(resp.Error, "too large") {
		t.Fatalf("error = %q, want a request-body-too-large error", resp.Error)
	}
}

// TestListenURL guards the startup log URL for both addr formats.
func TestListenURL(t *testing.T) {
	if got := listenURL(":8080"); got != "http://localhost:8080" {
		t.Errorf("listenURL(\":8080\") = %q, want http://localhost:8080", got)
	}
	if got := listenURL("127.0.0.1:8080"); got != "http://127.0.0.1:8080" {
		t.Errorf("listenURL(\"127.0.0.1:8080\") = %q, want http://127.0.0.1:8080", got)
	}
}

// TestJnumSanitizesNaNInf guards the JSON-safety of numeric outputs.
func TestJnumSanitizesNaNInf(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	if got := jnum(nan); got != 0 {
		t.Errorf("jnum(NaN) = %v, want 0", got)
	}
	if got := jnum(inf); got != 0 {
		t.Errorf("jnum(+Inf) = %v, want 0", got)
	}
	if got := jnum(1.239); got != 1.24 {
		t.Errorf("jnum(1.239) = %v, want 1.24", got)
	}
}

// TestNewSource proves source selection: yahoo needs no credentials, unknown
// names error, and the error text for a missing Oanda token never leaks one.
func TestNewSource(t *testing.T) {
	if _, err := newSource(runReq{}); err != nil {
		t.Errorf("default source: %v, want yahoo", err)
	}
	if _, err := newSource(runReq{Source: "yahoo"}); err != nil {
		t.Errorf("yahoo: %v", err)
	}
	if _, err := newSource(runReq{Source: "bloomberg"}); err == nil {
		t.Error("unknown source: want error")
	}
}

// TestNewSourceT212 proves the Trading 212 source needs a key pair (from env
// or .env files), errors helpfully without leaking values, and builds once
// credentials are present. Order matters: the missing-credentials case must
// run before the cached singleton is populated.
func TestNewSourceT212(t *testing.T) {
	t.Setenv("T212_API_KEY", "")
	t.Setenv("T212_API_SECRET", "")
	_, err := newSource(runReq{Source: "t212"})
	if err == nil || !strings.Contains(err.Error(), "T212_API_KEY") {
		t.Fatalf("t212 without creds: err = %v, want a credential error", err)
	}

	t.Setenv("T212_API_KEY", "test-key")
	t.Setenv("T212_API_SECRET", "test-secret")
	if _, err := newSource(runReq{Source: "t212"}); err != nil {
		t.Fatalf("t212 with creds: %v", err)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Error("credential error text must never contain values")
	}
}

// TestBuildStrategy proves strategy selection and its validation paths.
func TestBuildStrategy(t *testing.T) {
	valid := []condReq{{Indicator: "rsi", Op: "<", P1: 14}}
	if _, err := buildStrategy(runReq{Conditions: valid}); err != nil {
		t.Errorf("default conditions strategy: %v", err)
	}
	if _, err := buildStrategy(runReq{Strategy: "conditions"}); err == nil {
		t.Error("conditions strategy with no conditions: want error")
	}
	if _, err := buildStrategy(runReq{Strategy: "wr"}); err != nil {
		t.Errorf("wr strategy with defaults: %v", err)
	}
	if _, err := buildStrategy(runReq{Strategy: "wrema", WRPeriod: 21, EMAPeriod: 12}); err != nil {
		t.Errorf("wrema strategy: %v", err)
	}
	if _, err := buildStrategy(runReq{Strategy: "wr", WRPeriod: 5000}); err == nil {
		t.Error("absurd period: want error")
	}
	if _, err := buildStrategy(runReq{Strategy: "macd-cross"}); err == nil {
		t.Error("unknown strategy: want error")
	}
}

// TestChartTime proves intraday bars serialize as UNIX seconds and daily bars
// as date strings (both Lightweight Charts time formats).
func TestChartTime(t *testing.T) {
	ts := time.Date(2026, 7, 15, 13, 30, 0, 0, time.UTC)
	if got := chartTime(ts, false); got != "2026-07-15" {
		t.Errorf("daily = %v", got)
	}
	if got := chartTime(ts, true); got != ts.Unix() {
		t.Errorf("intraday = %v, want %d", got, ts.Unix())
	}
	for iv, want := range map[string]bool{"": false, "1d": false, "1wk": false, "1mo": false, "10m": true, "1h": true, "4h": true} {
		if got := intradayInterval(iv); got != want {
			t.Errorf("intradayInterval(%q) = %v, want %v", iv, got, want)
		}
	}
}

// TestCostValidation proves the costs & leverage inputs are validated
// server-side before any data is fetched.
func TestCostValidation(t *testing.T) {
	base := runReq{TP: 10, Strategy: "wr"}
	cases := []struct {
		name string
		mut  func(*runReq)
		want string
	}{
		{"negative spread", func(r *runReq) { r.SpreadPts = -1 }, "spread"},
		{"absurd financing", func(r *runReq) { r.FinRatePct = 90 }, "financing"},
		{"excess leverage", func(r *runReq) { r.Leverage = 500 }, "leverage"},
		{"fractional sub-1 leverage", func(r *runReq) { r.Leverage = 0.5 }, "leverage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mut(&req)
			resp := runBacktest(req)
			if resp.Error == "" || !strings.Contains(resp.Error, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", resp.Error, tc.want)
			}
		})
	}
}

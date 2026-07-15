module github.com/florinel-chis/gobacktest-ui

go 1.26.5

require (
	github.com/florinel-chis/gobacktest v0.0.0
	github.com/florinel-chis/oanda-go v0.0.0
	github.com/florinel-chis/oanda-go/backtestsource v0.0.0
	github.com/florinel-chis/yahoo-go/backtestsource v0.0.0
)

require (
	github.com/florinel-chis/yahoo-go v0.0.0 // indirect
	github.com/markcheno/go-talib v0.0.0-20260617004048-4ff5e103edd9 // indirect
)

replace (
	github.com/florinel-chis/gobacktest => /Users/fch/repos/gobacktest
	github.com/florinel-chis/oanda-go => /Users/fch/repos/oanda-go
	github.com/florinel-chis/oanda-go/backtestsource => /Users/fch/repos/oanda-go/backtestsource
	github.com/florinel-chis/yahoo-go => /Users/fch/repos/yahoo-go
	github.com/florinel-chis/yahoo-go/backtestsource => /Users/fch/repos/yahoo-go/backtestsource
)

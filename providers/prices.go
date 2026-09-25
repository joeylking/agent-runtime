package providers

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
)

// ErrNoPrice is wrapped by PriceFor for a name the table does not hold. A
// paid model runs only with a known price.
var ErrNoPrice = errors.New("no price for this model")

// PriceFor returns the price recorded for a model. A name absent from the
// table is an error rather than a price of zero: agentrt.PriceTable charges
// nothing for an unknown name, which is right for a local model and would
// silently defeat a budget for a paid one. Local models are recorded with
// Free so that "free" and "unpriced" stay distinguishable.
func PriceFor(prices agentrt.PriceTable, name string) (agentrt.Price, error) {
	p, ok := prices[name]
	if !ok {
		return agentrt.Price{}, fmt.Errorf("%s: %w", name, ErrNoPrice)
	}
	return p, nil
}

// Free records each name as costing nothing, which is what a local model
// costs. The zero prices make no difference to accounting; the entries make
// the difference to PriceFor.
func Free(names ...string) agentrt.PriceTable {
	t := make(agentrt.PriceTable, len(names))
	for _, n := range names {
		t[n] = agentrt.Price{}
	}
	return t
}

// Merge combines price tables into one for agentrt.ModelConfig, so a
// consumer with a local and a paid model passes a single table. Later
// tables win.
func Merge(tables ...agentrt.PriceTable) agentrt.PriceTable {
	out := agentrt.PriceTable{}
	for _, t := range tables {
		for name, p := range t {
			out[name] = p
		}
	}
	return out
}

// ParseDollars converts a budget written as "5", "4.41", or "$0.50" into
// micros, rounding to the nearest micro. Negative amounts are refused.
func ParseDollars(s string) (agentrt.Micros, error) {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("providers: invalid dollar amount %q", s)
	}
	return agentrt.Micros(f*1e6 + 0.5), nil
}

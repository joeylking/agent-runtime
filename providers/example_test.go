package providers_test

import (
	"errors"
	"fmt"

	"github.com/joeylking/agent-runtime/providers"
)

// PriceFor refuses a model the table does not hold, because agentrt.PriceTable
// charges nothing for an unknown name and that would silently defeat a budget.
// Free records a local model, so free and unpriced stay distinguishable.
func ExamplePriceFor() {
	prices := providers.Free("ollama:qwen3:30b-a3b")

	local, err := providers.PriceFor(prices, "ollama:qwen3:30b-a3b")
	fmt.Println(local.InputPerMTok, err)

	_, err = providers.PriceFor(prices, "openai:a-paid-model")
	fmt.Println(errors.Is(err, providers.ErrNoPrice), err)
	// Output:
	// 0 <nil>
	// true openai:a-paid-model: no price for this model
}

// ParseDollars reads a budget as a person writes it and returns micros.
func ExampleParseDollars() {
	for _, s := range []string{"5", "4.41", "$0.50"} {
		m, err := providers.ParseDollars(s)
		fmt.Println(s, m, err)
	}
	// Output:
	// 5 5000000 <nil>
	// 4.41 4410000 <nil>
	// $0.50 500000 <nil>
}

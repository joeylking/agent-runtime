module github.com/joeylking/agent-runtime/examples/live

// Held above the core module's go 1.26 because the core release this module
// pins, v0.2.0, declares go 1.27; go mod tidy raises this line to match. It
// drops to 1.26 when this module re-pins to a core release declaring 1.26.
go 1.27

require (
	// Sibling modules are pinned to their release tags; the workspace
	// the README builds overrides these for development inside the repository.
	github.com/joeylking/agent-runtime v0.2.0
	github.com/joeylking/agent-runtime/providers/ollama v0.1.0
	github.com/joeylking/agent-runtime/providers/openai v0.1.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
)

# Adapter conformance harness

This package defines a reusable contract test for all Overseer adapters.

Use `conformance.Run` in each adapter package to verify behavior for:

- stable non-empty adapter name
- panic-free execution with a no-op event sink
- happy-path execution (or expected failure for stub adapters)
- context cancellation responsiveness
- timeout responsiveness
- outcome-domain correctness
- optional chunked streaming output for stream-producing adapters

## One-line adoption example

```go
func TestMyAdapter_Conformance(t *testing.T) {
    conformance.Run(t, "my-adapter",
        func() adapter.Adapter { return myadapter.New() },
        conformance.Options{
            StepConfig: map[string]string{"prompt": "hello"},
            AllowedOutcomes: []string{"success", "failure", "needs_review"},
            Streaming: true,
        })
}
```

## Options

- `StepConfig`: step config map passed to the adapter under test.
- `AllowedOutcomes`: accepted `adapter.Result.Outcome` values when `Execute` returns no error.
- `Streaming`: enables the chunked IO conformance sub-test.
- `ExpectError`: matcher for expected-failure adapters (for example, the non-tagged copilot stub).

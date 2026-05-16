# Adapter conformance harness

This package defines a reusable contract test for all Criteria adapters.

Use `conformance.Run` for in-process adapters, and `conformance.RunAdapter` for out-of-process adapter binaries.

Shared contract sub-tests (both entry points):

- stable non-empty adapter name
- panic-free execution with a no-op event sink
- happy-path execution (or expected failure for adapters that intentionally fail)
- context cancellation responsiveness
- timeout responsiveness
- outcome-domain correctness
- optional chunked streaming output for stream-producing adapters

Adapter-only sub-tests (`RunAdapter`):

- `session_lifecycle`: open -> execute -> execute -> close, then verify execute-after-close errors.
- `concurrent_sessions`: open two sessions in parallel and verify per-session isolation.
- `session_crash_detection`: kill the adapter process and verify the next execute returns an error (no panic/hang).
- `permission_request_shape`: for adapters advertising `permission_gating`, verify `permission.request` event shape and deny -> `needs_review` outcome.

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

## Adapter adoption example

```go
func TestMyAdapter_Conformance(t *testing.T) {
    conformance.RunAdapter(t, "myadapter",
        filepath.Join("..", "..", "..", "bin", "criteria-adapter-myadapter"),
        conformance.Options{
            StepConfig: map[string]string{"prompt": "hello"},
            AllowedOutcomes: []string{"success", "failure", "needs_review"},
            Streaming: true,
        })
}
```

## Options

- `StepConfig`: step config map passed to the adapter under test.
- `PermissionConfig`: optional config used only by `permission_request_shape` (falls back to `StepConfig`).
- `AllowedOutcomes`: accepted `adapter.Result.Outcome` values when `Execute` returns no error.
- `Streaming`: enables the chunked IO conformance sub-test.
- `ExpectError`: matcher for expected-failure adapters.

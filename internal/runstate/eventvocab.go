package runstate

import "unicode"

// The ND-JSON producer (internal/run LocalSink) stamps each envelope with the
// PascalCase proto message name as payload_type; the run viewer's envelope
// type is the proto oneof case name, which is lowerCamelCase in the castle
// rendering (dataSource.ts TERMINAL_EVENT_TYPES = {runCompleted, runFailed}).
// payloadTypeToSeamType is the one shared table mapping every producer
// discriminator onto the seam vocabulary — no call site invents strings.
var payloadTypeToSeamType = map[string]string{
	"RunStarted":             "runStarted",
	"RunCompleted":           "runCompleted",
	"RunFailed":              "runFailed",
	"StepEntered":            "stepEntered",
	"StepOutcome":            "stepOutcome",
	"StepTransition":         "stepTransition",
	"StepLog":                "stepLog",
	"AdapterEvent":           "adapterEvent",
	"CriteriaHeartbeat":      "criteriaHeartbeat",
	"CriteriaDisconnected":   "criteriaDisconnected",
	"StepResumed":            "stepResumed",
	"VariableSet":            "variableSet",
	"StepOutputCaptured":     "stepOutputCaptured",
	"WaitEntered":            "waitEntered",
	"WaitResumed":            "waitResumed",
	"ApprovalRequested":      "approvalRequested",
	"ApprovalDecision":       "approvalDecision",
	"BranchEvaluated":        "branchEvaluated",
	"ForEachEntered":         "forEachEntered",
	"StepIterationStarted":   "stepIterationStarted",
	"StepIterationCompleted": "stepIterationCompleted",
	"StepIterationItem":      "stepIterationItem",
	"ScopeIterCursorSet":     "scopeIterCursorSet",
	"RunOutputs":             "runOutputs",
	"run.outputs":            "runOutputs", // historical LocalSink discriminator for RunOutputs
	"WatchReady":             "watchReady",
}

// TerminalSeamEventTypes is the seam's terminal vocabulary — the types the
// consumer's stream walk stops on. Membership is the vocabulary's contract:
// a run is only "done" to the viewer when one of these types is served.
var TerminalSeamEventTypes = map[string]bool{
	"runCompleted": true,
	"runFailed":    true,
}

// seamEventType maps a producer payload_type discriminator to the seam event
// type. Discriminators not in the table (a future producer event) fall back
// to a lower-camel rendering per dot-separated segment so they still land in
// the seam's camelCase vocabulary.
func seamEventType(payloadType string) string {
	if t, ok := payloadTypeToSeamType[payloadType]; ok {
		return t
	}
	return lowerCamelSegments(payloadType)
}

func lowerCamelSegments(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 || s[i-1] == '.' {
			out = append(out, byte(unicode.ToLower(rune(c))))
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

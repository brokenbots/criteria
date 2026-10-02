package conformance_test

// KB-45: the engine-repo consumer for the criteria-adapter-proto v0.7.0
// shared conformance vectors. The proto repo's own conformance suite pins
// byte-identity and linkage; this suite additionally re-applies the pinned
// evaluator (v2.EvaluateOutcomeContracts) to each vector phase so the
// ENGINE's validation path is proven against the same committed fixtures
// the three adapter SDKs consume.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brokenbots/criteria-adapter-proto/conformance"
	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var pinnedVectors = []string{
	"01_contract_roundtrip_valid.json",
	"02_contract_payload_invalid.json",
	"03_require_comment_missing.json",
	"04_fallback_fires.json",
	"05_no_contracts_legacy.json",
	"06_comment_end_to_end.json",
	"07_rejection_repair_context.json",
}

type vectorPhase struct {
	Request json.RawMessage   `json:"request"`
	Results []json.RawMessage `json:"results"`
	Expect  struct {
		Accepted  *bool           `json:"accepted"`
		Forwarded json.RawMessage `json:"forwarded"`
		Issues    []string        `json:"issues"`
		Rejection json.RawMessage `json:"rejection"`
	} `json:"expect"`
}

type vectorDoc struct {
	Name   string        `json:"name"`
	Phases []vectorPhase `json:"phases"`
}

func loadPinnedVectors(t *testing.T) map[string]vectorDoc {
	t.Helper()
	names, err := conformance.VectorNames()
	require.NoError(t, err)
	require.ElementsMatch(t, pinnedVectors, names, "the pinned vector inventory changed; update this consumer with the proto release")

	docs := make(map[string]vectorDoc, len(names))
	for _, name := range names {
		raw, err := conformance.ReadVector(name)
		require.NoError(t, err)
		var doc vectorDoc
		require.NoError(t, json.Unmarshal(raw, &doc), "vector %s", name)
		require.NotEmpty(t, doc.Name, "vector %s must carry a human label", name)
		require.NotEmpty(t, doc.Phases, "vector %s", name)
		docs[name] = doc
	}
	return docs
}

func unmarshalProtoJSON(t *testing.T, raw json.RawMessage, m proto.Message) {
	t.Helper()
	if len(raw) == 0 {
		return
	}
	require.NoError(t, protojson.Unmarshal(raw, m), "proto3 JSON parse of %.60s", raw)
}

// TestEngineConsumesPinnedVectors mirrors the proto repo's parse and
// rejection-linkage tests over the same bytes, then re-applies the pinned
// evaluator to each phase's request/results and asserts the accepted,
// forwarded, and issue expectations hold.
func TestEngineConsumesPinnedVectors(t *testing.T) {
	for name, doc := range loadPinnedVectors(t) {
		t.Run(name, func(t *testing.T) {
			var pendingRejection *criteriav2.ExecutionRejection
			for pi, phase := range doc.Phases {
				req := &criteriav2.ExecuteRequest{}
				unmarshalProtoJSON(t, phase.Request, req)

				// Rejection linkage: the request the repair loop re-sends
				// carries the prior phase's expected rejection verbatim.
				if pendingRejection != nil {
					require.True(t, proto.Equal(pendingRejection, req.GetRejection()),
						"phase %d request.rejection must carry phase %d expect.rejection verbatim", pi, pi-1)
				}

				results := make([]*criteriav2.ExecuteResult, 0, len(phase.Results))
				for _, raw := range phase.Results {
					r := &criteriav2.ExecuteResult{}
					unmarshalProtoJSON(t, raw, r)
					results = append(results, r)
				}

				evaluated, issues := criteriav2.EvaluateOutcomeContracts(req, results)
				if !*phase.Expect.Accepted {
					// The pinned issue list is authoritative and ordered;
					// the rejection's joined issues text is derived from it.
					require.Equal(t, issueList(phase.Expect.Issues), issues, "phase %d issues", pi)
					require.Nil(t, evaluated)
					if phase.Expect.Rejection != nil {
						rej := &criteriav2.ExecutionRejection{}
						unmarshalProtoJSON(t, phase.Expect.Rejection, rej)
						require.Equal(t, strings.Join(phase.Expect.Issues, "\n"), rej.GetIssues())
						pendingRejection = rej
					}
					continue
				}
				require.Empty(t, issues, "phase %d issues", pi)
				if phase.Expect.Forwarded != nil {
					want := &criteriav2.ExecuteResult{}
					unmarshalProtoJSON(t, phase.Expect.Forwarded, want)
					require.True(t, proto.Equal(want, evaluated), "phase %d forwarded", pi)
				}
				pendingRejection = nil
			}
		})
	}
}

func issueList(want []string) []string {
	if len(want) == 0 {
		return nil
	}
	return want
}

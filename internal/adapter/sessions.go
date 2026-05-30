package adapter

import (
	"fmt"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

// SessionChunkThreshold is the payload size above which session wire data
// (snapshot state, restore state, accumulated adapter events) must be split
// into Chunk envelopes.  It matches DefaultMaxChunkBytes from the chunking
// package so that both sides agree on the wire limit.
const SessionChunkThreshold = criteriav2.DefaultMaxChunkBytes

// ChunkSessionState splits a session state blob into ChunkEnvelope fragments.
// If data is smaller than SessionChunkThreshold it still returns a single
// envelope with seq=0, final=true, total=1 so the consumer path is uniform.
// payloadID is optional; when non-empty it is stamped on every envelope for
// future reconnect-resume support (WS19 Step 4).
func ChunkSessionState(data []byte, payloadID string) ([]*criteriav2.ChunkEnvelope, error) {
	var sink sliceChunkSink
	if err := criteriav2.SendChunks(data, payloadID, 0, &sink); err != nil {
		return nil, fmt.Errorf("chunk session state: %w", err)
	}
	return sink.envs, nil
}

// UnchunkSessionState reassembles a session state blob from ChunkSource until
// the final flag is seen.  It validates ordering and rejects duplicates, just
// like AssembleChunks.  The optional payloadID argument filters the stream to
// a single logical payload.
func UnchunkSessionState(src criteriav2.ChunkSource, payloadID string) ([]byte, error) {
	data, err := criteriav2.AssembleChunks(src, payloadID)
	if err != nil {
		return nil, fmt.Errorf("unchunk session state: %w", err)
	}
	return data, nil
}

// sliceChunkSink is a trivial in-memory ChunkSink used by ChunkSessionState.
type sliceChunkSink struct {
	envs []*criteriav2.ChunkEnvelope
}

func (s *sliceChunkSink) Send(env *criteriav2.ChunkEnvelope) error {
	s.envs = append(s.envs, env)
	return nil
}

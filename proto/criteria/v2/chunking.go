package criteriav2

import (
	"errors"
	"fmt"
)

const (
	// DefaultMaxChunkBytes is the protocol default maximum payload size for a
	// single message field before chunking is required (4 MiB).
	DefaultMaxChunkBytes uint32 = 4 * 1024 * 1024
)

// NegotiateChunkSize returns the effective maximum chunk size given the
// adapter's declared limit and the host's own limit.
//
// Either value may be zero, meaning "use the protocol default (4 MiB)".
// The effective limit is min(adapterMax, hostMax) after replacing any zero
// with DefaultMaxChunkBytes.
func NegotiateChunkSize(adapterMax, hostMax uint32) uint32 {
	if adapterMax == 0 {
		adapterMax = DefaultMaxChunkBytes
	}
	if hostMax == 0 {
		hostMax = DefaultMaxChunkBytes
	}
	if adapterMax < hostMax {
		return adapterMax
	}
	return hostMax
}

// SplitChunks is the low-level bytes splitter.  It splits data into a slice of
// Chunk messages whose payload size does not exceed chunkSize bytes.
// chunkSize == 0 uses DefaultMaxChunkBytes.
//
// The returned Chunk messages carry only the framing metadata (seq, total,
// final).  Callers embed the corresponding data slice (returned as the second
// return value) into whatever payload field is being chunked.
//
// Splitting is done at arbitrary byte boundaries.  Callers that need to
// preserve encoding invariants (e.g. codepoint-aligned UTF-8) must do so
// before calling this function.  For the three officially chunkable fields in
// this package, all splitting goes through the high-level helpers:
// ChunkAdapterEventPayload, ChunkExecuteResultOutputs, and ChunkLogEventLine.
// Those helpers are the only officially supported callers of SplitChunks.
//
// If len(data) == 0 a single empty chunk is returned.
func SplitChunks(data []byte, chunkSize uint32) (chunks []*Chunk, payloads [][]byte) {
	if chunkSize == 0 {
		chunkSize = DefaultMaxChunkBytes
	}
	if len(data) == 0 {
		return []*Chunk{{Seq: 0, Total: 1, Final: true}}, [][]byte{nil}
	}

	for off := 0; off < len(data); off += int(chunkSize) {
		end := off + int(chunkSize)
		if end > len(data) {
			end = len(data)
		}
		payloads = append(payloads, data[off:end])
	}

	total := uint32(len(payloads))
	chunks = make([]*Chunk, total)
	for i := range payloads {
		chunks[i] = &Chunk{
			Seq:   uint32(i),
			Total: total,
			Final: uint32(i) == total-1,
		}
	}
	return chunks, payloads
}

// NeedsChunking reports whether data exceeds the negotiated chunk size and
// therefore requires multi-message framing.
func NeedsChunking(data []byte, negotiatedMax uint32) bool {
	if negotiatedMax == 0 {
		negotiatedMax = DefaultMaxChunkBytes
	}
	return len(data) > int(negotiatedMax)
}

// DefaultChunkSize is the default size of each chunk emitted by SendChunks.
// It is smaller than DefaultMaxChunkBytes so that even payloads just above the
// 4 MiB threshold are split into manageable fragments.
const DefaultChunkSize uint32 = 1 * 1024 * 1024

// ChunkEnvelope carries a Chunk, its payload bytes, and an optional payload
// identifier for reconnect-safe reassembly (WS19 Step 4).  payload_id is
// reserved for future wire use; it is not serialized into the proto Chunk
// message today, but callers may carry it out-of-band.
type ChunkEnvelope struct {
	Chunk     *Chunk
	Payload   []byte
	PayloadID string
}

// ChunkSink receives chunked fragments.
type ChunkSink interface {
	Send(env *ChunkEnvelope) error
}

// ChunkSource yields chunked fragments.
type ChunkSource interface {
	Recv() (*ChunkEnvelope, error)
}

// SendChunks splits body into ChunkEnvelope fragments and emits them on sink.
// chunkSize == 0 uses DefaultChunkSize (1 MiB).  payloadID is optional; it is
// copied into every envelope for future reconnect-resume support.
func SendChunks(body []byte, payloadID string, chunkSize uint32, sink ChunkSink) error {
	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}
	chunks, payloads := SplitChunks(body, chunkSize)
	for i, c := range chunks {
		if err := sink.Send(&ChunkEnvelope{
			Chunk:     c,
			Payload:   payloads[i],
			PayloadID: payloadID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// AssembleChunks accumulates chunks from stream until the final flag is seen
// and returns the reassembled body.  It validates sequential ordering,
// rejects duplicate seq values, and errors if the stream ends without a final
// chunk.  The optional payloadID argument filters the stream to a single
// logical payload; when empty, every chunk is accepted (legacy behaviour).
func AssembleChunks(stream ChunkSource, payloadID string) ([]byte, error) {
	var state assembleState

	for {
		env, err := recvChunk(stream, payloadID)
		if err != nil {
			if errors.Is(err, ErrChunkStreamClosed) {
				if state.inProgress {
					return nil, fmt.Errorf("chunk stream closed without final chunk (seq %d/%d)", state.expectSeq, state.lastTotal)
				}
				return state.buf, nil
			}
			return nil, err
		}

		c := env.Chunk
		if err := state.accept(c.GetSeq(), c.GetTotal()); err != nil {
			return nil, err
		}

		state.buf = append(state.buf, env.Payload...)

		if c.GetFinal() {
			if state.expectSeq != state.lastTotal {
				return nil, fmt.Errorf("chunk final flag with mismatched total: expected %d chunks, got %d", state.lastTotal, state.expectSeq)
			}
			return state.buf, nil
		}
	}
}

// assembleState tracks the reassembly progress for AssembleChunks.
type assembleState struct {
	buf        []byte
	expectSeq  uint32
	inProgress bool
	lastTotal  uint32
}

// accept validates the next chunk sequence number and updates state.
func (s *assembleState) accept(seq, total uint32) error {
	if seq == 0 {
		s.buf = nil
		s.expectSeq = 1
		s.inProgress = true
		s.lastTotal = total
		return nil
	}
	if !s.inProgress {
		return fmt.Errorf("chunk seq %d received with no sequence in progress", seq)
	}
	if seq != s.expectSeq {
		return fmt.Errorf("chunk out-of-order: got seq %d, expected %d", seq, s.expectSeq)
	}
	s.expectSeq = seq + 1
	return nil
}

// recvChunk reads the next envelope from stream and filters out non-chunk
// envelopes or envelopes belonging to a different payload.
func recvChunk(stream ChunkSource, payloadID string) (*ChunkEnvelope, error) {
	for {
		env, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if env.Chunk == nil {
			continue // skip non-chunk envelopes
		}
		if payloadID != "" && env.PayloadID != payloadID {
			continue // skip envelopes for other payloads
		}
		return env, nil
	}
}

// ErrChunkStreamClosed is returned by ChunkSource implementations to signal
// graceful end-of-stream.  AssembleChunks treats it as a normal terminator
// when no reassembly is in progress.
var ErrChunkStreamClosed = errors.New("chunk stream closed")

// ─── Structured chunking helpers ────────────────────────────────────────────
//
// These helpers implement the on-wire chunking contract for AdapterEvent and
// ExecuteResult payloads: the caller serialises the typed value to JSON bytes,
// passes those bytes here, and receives a slice of proto messages ready to send
// on the Execute stream.  The receiver reassembles by joining the *_json fields
// and unmarshalling back to the typed form.

// ChunkAdapterEventPayload splits payloadJSON into fragment AdapterEvent
// messages derived from base (event_kind and emitted_at are preserved).
// Each fragment has chunk set and payload_json set to the fragment bytes;
// payload is left nil because the JSON is split across multiple messages.
// chunkSize == 0 uses DefaultMaxChunkBytes.
func ChunkAdapterEventPayload(base *AdapterEvent, payloadJSON []byte, chunkSize uint32) []*AdapterEvent {
	chunks, payloads := SplitChunks(payloadJSON, chunkSize)
	result := make([]*AdapterEvent, len(chunks))
	for i, c := range chunks {
		result[i] = &AdapterEvent{
			EventKind:   base.EventKind,
			EmittedAt:   base.EmittedAt,
			Chunk:       c,
			PayloadJson: payloads[i],
		}
	}
	return result
}

// JoinAdapterEventPayload reassembles payload_json bytes from a sequence of
// AdapterEvent fragment messages (ordered by Chunk.Seq) and returns the
// concatenated JSON bytes.  The caller is responsible for unmarshalling the
// result (e.g. via protojson or structpb.Struct.UnmarshalJSON).
//
// Returns an error if any message in events lacks Chunk metadata.
func JoinAdapterEventPayload(events []*AdapterEvent) ([]byte, error) {
	if len(events) == 0 {
		return nil, errors.New("no AdapterEvent fragments to join")
	}
	var buf []byte
	for i, ev := range events {
		if ev.Chunk == nil {
			return nil, fmt.Errorf("AdapterEvent fragment[%d] has no Chunk metadata", i)
		}
		buf = append(buf, ev.PayloadJson...)
	}
	return buf, nil
}

// ChunkExecuteResultOutputs splits outputsJSON into fragment ExecuteResult
// messages derived from base (outcome is preserved).  Each fragment has chunk
// set and outputs_json set to the fragment bytes; outputs is left nil.
// chunkSize == 0 uses DefaultMaxChunkBytes.
func ChunkExecuteResultOutputs(base *ExecuteResult, outputsJSON []byte, chunkSize uint32) []*ExecuteResult {
	chunks, payloads := SplitChunks(outputsJSON, chunkSize)
	result := make([]*ExecuteResult, len(chunks))
	for i, c := range chunks {
		result[i] = &ExecuteResult{
			Outcome:     base.Outcome,
			Chunk:       c,
			OutputsJson: payloads[i],
		}
	}
	return result
}

// JoinExecuteResultOutputs reassembles outputs_json bytes from a sequence of
// ExecuteResult fragment messages (ordered by Chunk.Seq) and returns the
// concatenated JSON bytes.  The caller is responsible for unmarshalling the
// result (e.g. via encoding/json into a map[string]string).
//
// Returns an error if any message in events lacks Chunk metadata.
func JoinExecuteResultOutputs(events []*ExecuteResult) ([]byte, error) {
	if len(events) == 0 {
		return nil, errors.New("no ExecuteResult fragments to join")
	}
	var buf []byte
	for i, ev := range events {
		if ev.Chunk == nil {
			return nil, fmt.Errorf("ExecuteResult fragment[%d] has no Chunk metadata", i)
		}
		buf = append(buf, ev.OutputsJson...)
	}
	return buf, nil
}

// ChunkLogEventLine splits a long log line from base into multiple LogEvent
// fragment messages.  base fields session_id, step_name, stream_name, and
// timestamp are copied to every fragment; each fragment carries a portion of
// the line bytes in its line field plus Chunk framing metadata.
//
// Because LogEvent.line is bytes, splitting is done at arbitrary byte
// boundaries using SplitChunks.  chunkSize == 0 uses DefaultMaxChunkBytes.
func ChunkLogEventLine(base *LogEvent, chunkSize uint32) []*LogEvent {
	chunks, payloads := SplitChunks(base.Line, chunkSize)
	result := make([]*LogEvent, len(chunks))
	for i, c := range chunks {
		result[i] = &LogEvent{
			SessionId:  base.SessionId,
			StepName:   base.StepName,
			StreamName: base.StreamName,
			Timestamp:  base.Timestamp,
			Line:       payloads[i],
			Chunk:      c,
		}
	}
	return result
}

// JoinLogEventLine reassembles the line field from a sequence of LogEvent
// fragment messages (ordered by Chunk.Seq) and returns the full log line bytes.
//
// Returns an error if any message in events lacks Chunk metadata.
func JoinLogEventLine(events []*LogEvent) ([]byte, error) {
	if len(events) == 0 {
		return nil, errors.New("no LogEvent fragments to join")
	}
	var buf []byte
	for i, ev := range events {
		if ev.Chunk == nil {
			return nil, fmt.Errorf("LogEvent fragment[%d] has no Chunk metadata", i)
		}
		buf = append(buf, ev.Line...)
	}
	return buf, nil
}

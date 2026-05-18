package criteriav2

import (
	"errors"
	"fmt"
	"unicode/utf8"
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

// SplitChunks splits data into a slice of Chunk messages whose payload size
// does not exceed chunkSize bytes.  chunkSize == 0 uses DefaultMaxChunkBytes.
//
// The returned Chunk messages carry only the framing metadata (seq, total,
// final).  Callers embed the corresponding data slice (returned as the second
// return value) into whatever payload field is being chunked.
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
	return uint32(len(data)) > negotiatedMax
}

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
// the line string in its line field plus Chunk framing metadata.
//
// Splitting is done at rune boundaries so every fragment is valid UTF-8 and
// safe to carry in a protobuf string field.  chunkSize == 0 uses DefaultMaxChunkBytes.
func ChunkLogEventLine(base *LogEvent, chunkSize uint32) []*LogEvent {
	if chunkSize == 0 {
		chunkSize = DefaultMaxChunkBytes
	}
	// Split at rune boundaries to guarantee valid UTF-8 in each fragment.
	var fragments []string
	line := base.Line
	for line != "" {
		if uint32(len(line)) <= chunkSize {
			fragments = append(fragments, line)
			break
		}
		// Walk back from the byte limit to find the preceding rune boundary.
		end := int(chunkSize)
		for end > 0 && !utf8.RuneStart(line[end]) {
			end--
		}
		if end == 0 {
			// A single rune is wider than chunkSize; emit it as one fragment.
			_, size := utf8.DecodeRuneInString(line)
			end = size
		}
		fragments = append(fragments, line[:end])
		line = line[end:]
	}
	if len(fragments) == 0 {
		fragments = []string{""}
	}
	total := uint32(len(fragments))
	result := make([]*LogEvent, total)
	for i, frag := range fragments {
		result[i] = &LogEvent{
			SessionId:  base.SessionId,
			StepName:   base.StepName,
			StreamName: base.StreamName,
			Timestamp:  base.Timestamp,
			Line:       frag,
			Chunk: &Chunk{
				Seq:   uint32(i),
				Total: total,
				Final: uint32(i) == total-1,
			},
		}
	}
	return result
}

// JoinLogEventLine reassembles the line field from a sequence of LogEvent
// fragment messages (ordered by Chunk.Seq) and returns the full log line.
//
// Returns an error if any message in events lacks Chunk metadata.
func JoinLogEventLine(events []*LogEvent) (string, error) {
	if len(events) == 0 {
		return "", errors.New("no LogEvent fragments to join")
	}
	var buf []byte
	for i, ev := range events {
		if ev.Chunk == nil {
			return "", fmt.Errorf("LogEvent fragment[%d] has no Chunk metadata", i)
		}
		buf = append(buf, []byte(ev.Line)...)
	}
	return string(buf), nil
}

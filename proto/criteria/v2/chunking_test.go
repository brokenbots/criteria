package criteriav2_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

func TestNegotiateChunkSize(t *testing.T) {
	tests := []struct {
		name       string
		adapterMax uint32
		hostMax    uint32
		want       uint32
	}{
		{"both zero uses default", 0, 0, criteriav2.DefaultMaxChunkBytes},
		{"adapter smaller than host", 1 * 1024 * 1024, 4 * 1024 * 1024, 1 * 1024 * 1024},
		{"host smaller than adapter", 4 * 1024 * 1024, 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"adapter zero host set", 0, 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"host zero adapter set", 1 * 1024 * 1024, 0, 1 * 1024 * 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := criteriav2.NegotiateChunkSize(tc.adapterMax, tc.hostMax)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestNegotiateChunkSize_AdapterSmallHostLarge mirrors the spec example:
// adapter_max=1MiB, host_max=4MiB → effective = 1MiB.
func TestNegotiateChunkSize_AdapterSmallHostLarge(t *testing.T) {
	adapterMax := uint32(1 * 1024 * 1024)
	hostMax := uint32(4 * 1024 * 1024)
	got := criteriav2.NegotiateChunkSize(adapterMax, hostMax)
	assert.Equal(t, adapterMax, got, "adapter_max=1MiB wins over host_max=4MiB")
}

func TestSplitChunks_SmallPayload_NoSplit(t *testing.T) {
	data := []byte("hello")
	chunks, payloads := criteriav2.SplitChunks(data, 1024)
	require.Len(t, chunks, 1)
	assert.Equal(t, uint32(0), chunks[0].Seq)
	assert.Equal(t, uint32(1), chunks[0].Total)
	assert.True(t, chunks[0].Final)
	assert.Equal(t, data, payloads[0])
}

func TestSplitChunks_ExactlyOneChunk(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	chunks, payloads := criteriav2.SplitChunks(data, 100)
	require.Len(t, chunks, 1)
	assert.True(t, chunks[0].Final)
	assert.Equal(t, data, payloads[0])
}

func TestSplitChunks_MultipleChunks(t *testing.T) {
	// 10 bytes split into 3-byte chunks → 4 chunks.
	data := []byte("0123456789")
	chunks, payloads := criteriav2.SplitChunks(data, 3)
	require.Len(t, chunks, 4)
	assert.Equal(t, uint32(4), chunks[0].Total)

	var reassembled []byte
	for i, c := range chunks {
		assert.Equal(t, uint32(i), c.Seq)
		reassembled = append(reassembled, payloads[i]...)
	}
	assert.True(t, chunks[3].Final)
	assert.False(t, chunks[2].Final)
	assert.Equal(t, data, reassembled)
}

func TestSplitChunks_EmptyData(t *testing.T) {
	chunks, payloads := criteriav2.SplitChunks(nil, 1024)
	require.Len(t, chunks, 1)
	assert.True(t, chunks[0].Final)
	assert.Nil(t, payloads[0])
}

func TestSplitChunks_ZeroChunkSize_UsesDefault(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 100)
	chunks, _ := criteriav2.SplitChunks(data, 0)
	// 100 bytes < 4 MiB default → one chunk.
	require.Len(t, chunks, 1)
}

// TestSplitChunks_LargePayload tests a payload >= 1MiB with a 1MiB chunk
// size (the spec example: payloads ≥1MiB split when adapter_max=1MiB).
func TestSplitChunks_LargePayload_GetsChunked(t *testing.T) {
	oneMiB := uint32(1 * 1024 * 1024)
	data := bytes.Repeat([]byte("z"), int(oneMiB)+1) // just over 1 MiB
	negotiated := criteriav2.NegotiateChunkSize(oneMiB, 4*1024*1024)
	assert.Equal(t, oneMiB, negotiated)

	chunks, payloads := criteriav2.SplitChunks(data, negotiated)
	assert.Greater(t, len(chunks), 1, "payload >= 1MiB must be split into multiple chunks")

	var reassembled []byte
	for _, p := range payloads {
		reassembled = append(reassembled, p...)
	}
	assert.Equal(t, data, reassembled)
}

func TestNeedsChunking(t *testing.T) {
	assert.False(t, criteriav2.NeedsChunking([]byte("small"), 1024))
	assert.True(t, criteriav2.NeedsChunking(bytes.Repeat([]byte("x"), 1025), 1024))
	assert.False(t, criteriav2.NeedsChunking(bytes.Repeat([]byte("x"), 1024), 1024))
}

// ─── SendChunks / AssembleChunks round-trip tests ───────────────────────────

// TestSendChunks_SubThreshold_NotChunked verifies the regression requirement:
// payloads below the 4 MiB threshold are delivered as a single unchunked
// envelope when chunked at the threshold size itself, so local UDS paths work
// unchanged.
func TestSendChunks_SubThreshold_NotChunked(t *testing.T) {
	data := bytes.Repeat([]byte{0xCD}, 4<<20-1) // 1 byte under threshold
	chunks, payloads := criteriav2.SplitChunks(data, criteriav2.DefaultMaxChunkBytes)
	require.Len(t, chunks, 1, "sub-threshold payload must produce exactly one chunk at threshold size")
	assert.True(t, chunks[0].Final, "single chunk must be final")
	assert.Equal(t, data, payloads[0], "single chunk must carry full payload")

	assert.False(t, criteriav2.NeedsChunking(data, criteriav2.DefaultMaxChunkBytes), "NeedsChunking must be false for sub-threshold payload")
}

func TestSendChunks_AssembleChunks_RoundTrip(t *testing.T) {
	sizes := []int{
		0,         // empty
		1,         // 1 byte
		1 << 20,   // 1 MiB
		4 << 20,   // 4 MiB
		16 << 20,  // 16 MiB
		100 << 20, // 100 MiB
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			var data []byte
			if size > 0 {
				data = bytes.Repeat([]byte{0xAB}, size)
			}
			sink := &sliceChunkSink{payloadID: "pid-1"}
			err := criteriav2.SendChunks(data, sink.payloadID, 0, sink)
			require.NoError(t, err)

			src := &sliceChunkSource{envelopes: sink.envelopes}
			got, err := criteriav2.AssembleChunks(src, "pid-1")
			require.NoError(t, err)
			assert.Equal(t, data, got, "reassembled payload must match original")
		})
	}
}

func TestAssembleChunks_OutOfOrder(t *testing.T) {
	data := []byte("0123456789")
	sink := &sliceChunkSink{payloadID: "pid"}
	err := criteriav2.SendChunks(data, sink.payloadID, 3, sink)
	require.NoError(t, err)

	// Swap last two chunks.
	sink.envelopes[2], sink.envelopes[3] = sink.envelopes[3], sink.envelopes[2]

	src := &sliceChunkSource{envelopes: sink.envelopes}
	_, err = criteriav2.AssembleChunks(src, "pid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out-of-order")
}

// TestAssembleChunks_DuplicateSeq verifies that a duplicate sequence number
// is detected via the out-of-order path (duplicate seq is a special case of
// out-of-order because the expected next seq has already passed).
func TestAssembleChunks_DuplicateSeq(t *testing.T) {
	data := []byte("0123456789")
	sink := &sliceChunkSink{payloadID: "pid"}
	err := criteriav2.SendChunks(data, sink.payloadID, 3, sink)
	require.NoError(t, err)

	// Duplicate the second chunk.
	sink.envelopes = append(sink.envelopes[:2], sink.envelopes[1:]...)

	src := &sliceChunkSource{envelopes: sink.envelopes}
	_, err = criteriav2.AssembleChunks(src, "pid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out-of-order")
}

func TestAssembleChunks_MissingFinal(t *testing.T) {
	data := []byte("0123456789")
	sink := &sliceChunkSink{payloadID: "pid"}
	err := criteriav2.SendChunks(data, sink.payloadID, 3, sink)
	require.NoError(t, err)

	// Drop the final chunk.
	sink.envelopes = sink.envelopes[:len(sink.envelopes)-1]

	src := &sliceChunkSource{envelopes: sink.envelopes}
	_, err = criteriav2.AssembleChunks(src, "pid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed without final")
}

func TestAssembleChunks_NoSequenceInProgress(t *testing.T) {
	src := &sliceChunkSource{envelopes: []*criteriav2.ChunkEnvelope{
		{Chunk: &criteriav2.Chunk{Seq: 5, Total: 10, Final: false}, Payload: []byte("x")},
	}}
	_, err := criteriav2.AssembleChunks(src, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sequence in progress")
}

func TestAssembleChunks_PayloadIDFiltering(t *testing.T) {
	sinkA := &sliceChunkSink{payloadID: "A"}
	sinkB := &sliceChunkSink{payloadID: "B"}

	dataA := []byte("payload-a")
	dataB := []byte("payload-b")
	require.NoError(t, criteriav2.SendChunks(dataA, "A", 3, sinkA))
	require.NoError(t, criteriav2.SendChunks(dataB, "B", 3, sinkB))

	// Interleave envelopes from both payloads.
	var mixed []*criteriav2.ChunkEnvelope
	for i := 0; i < max(len(sinkA.envelopes), len(sinkB.envelopes)); i++ {
		if i < len(sinkA.envelopes) {
			mixed = append(mixed, sinkA.envelopes[i])
		}
		if i < len(sinkB.envelopes) {
			mixed = append(mixed, sinkB.envelopes[i])
		}
	}

	src := &sliceChunkSource{envelopes: mixed}
	gotA, err := criteriav2.AssembleChunks(src, "A")
	require.NoError(t, err)
	assert.Equal(t, dataA, gotA)

	// Re-create source for B.
	src = &sliceChunkSource{envelopes: mixed}
	gotB, err := criteriav2.AssembleChunks(src, "B")
	require.NoError(t, err)
	assert.Equal(t, dataB, gotB)
}

func TestAssembleChunks_EmptyPayload(t *testing.T) {
	sink := &sliceChunkSink{payloadID: "pid"}
	err := criteriav2.SendChunks(nil, sink.payloadID, 0, sink)
	require.NoError(t, err)
	require.Len(t, sink.envelopes, 1)

	src := &sliceChunkSource{envelopes: sink.envelopes}
	got, err := criteriav2.AssembleChunks(src, "pid")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// sliceChunkSink is a test-only ChunkSink that captures envelopes.
type sliceChunkSink struct {
	envelopes []*criteriav2.ChunkEnvelope
	payloadID string
}

func (s *sliceChunkSink) Send(env *criteriav2.ChunkEnvelope) error {
	s.envelopes = append(s.envelopes, env)
	return nil
}

// sliceChunkSource is a test-only ChunkSource that replays captured envelopes.
type sliceChunkSource struct {
	envelopes []*criteriav2.ChunkEnvelope
	idx       int
}

func (s *sliceChunkSource) Recv() (*criteriav2.ChunkEnvelope, error) {
	if s.idx >= len(s.envelopes) {
		return nil, criteriav2.ErrChunkStreamClosed
	}
	env := s.envelopes[s.idx]
	s.idx++
	return env, nil
}

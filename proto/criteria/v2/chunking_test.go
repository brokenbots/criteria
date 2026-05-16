package criteriav2_test

import (
	"bytes"
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
		wantMin    uint32
		wantMax    uint32
	}{
		{"both zero uses default", 0, 0, criteriav2.DefaultMaxChunkBytes, criteriav2.DefaultMaxChunkBytes},
		{"adapter smaller than host", 1 * 1024 * 1024, 4 * 1024 * 1024, 1 * 1024 * 1024, 1 * 1024 * 1024},
		{"host smaller than adapter", 4 * 1024 * 1024, 2 * 1024 * 1024, 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"adapter zero host set", 0, 2 * 1024 * 1024, 2 * 1024 * 1024, 2 * 1024 * 1024},
		{"host zero adapter set", 1 * 1024 * 1024, 0, 1 * 1024 * 1024, 1 * 1024 * 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := criteriav2.NegotiateChunkSize(tc.adapterMax, tc.hostMax)
			assert.Equal(t, tc.wantMin, got)
			assert.Equal(t, tc.wantMax, got)
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

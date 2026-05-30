package adapter_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter"
	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

func TestChunkSessionState_RoundTrip(t *testing.T) {
	sizes := []int{0, 1, 1 << 20, 4 << 20, 16 << 20}
	for _, size := range sizes {
		t.Run("size", func(t *testing.T) {
			var data []byte
			if size > 0 {
				data = bytes.Repeat([]byte{0xCD}, size)
			}
			envs, err := adapter.ChunkSessionState(data, "pid-1")
			require.NoError(t, err)
			require.NotEmpty(t, envs)

			src := &sliceChunkSource{envs: envs}
			got, err := adapter.UnchunkSessionState(src, "pid-1")
			require.NoError(t, err)
			assert.Equal(t, data, got)
		})
	}
}

func TestChunkSessionState_PayloadIDOptional(t *testing.T) {
	data := []byte("hello-world")
	envs, err := adapter.ChunkSessionState(data, "")
	require.NoError(t, err)
	require.Len(t, envs, 1)
	assert.Empty(t, envs[0].PayloadID)

	src := &sliceChunkSource{envs: envs}
	got, err := adapter.UnchunkSessionState(src, "")
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestUnchunkSessionState_OutOfOrder(t *testing.T) {
	// Use a payload larger than the default 1 MiB chunk size so it splits.
	data := bytes.Repeat([]byte{0x01}, int(adapter.SessionChunkThreshold)+10)
	envs, err := adapter.ChunkSessionState(data, "pid")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(envs), 2)

	// Swap last two chunks.
	last := len(envs) - 1
	envs[last-1], envs[last] = envs[last], envs[last-1]

	src := &sliceChunkSource{envs: envs}
	_, err = adapter.UnchunkSessionState(src, "pid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out-of-order")
}

func TestUnchunkSessionState_MissingFinal(t *testing.T) {
	data := bytes.Repeat([]byte{0x01}, int(adapter.SessionChunkThreshold)+10)
	envs, err := adapter.ChunkSessionState(data, "pid")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(envs), 2)

	// Drop the final chunk.
	envs = envs[:len(envs)-1]

	src := &sliceChunkSource{envs: envs}
	_, err = adapter.UnchunkSessionState(src, "pid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "final")
}

// sliceChunkSource is a test-only ChunkSource that replays captured envelopes.
type sliceChunkSource struct {
	envs []*criteriav2.ChunkEnvelope
	idx  int
}

func (s *sliceChunkSource) Recv() (*criteriav2.ChunkEnvelope, error) {
	if s.idx >= len(s.envs) {
		return nil, criteriav2.ErrChunkStreamClosed
	}
	env := s.envs[s.idx]
	s.idx++
	return env, nil
}

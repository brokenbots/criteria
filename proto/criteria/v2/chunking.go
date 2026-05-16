package criteriav2

const (
	// DefaultMaxChunkBytes is the protocol default maximum payload size for a
	// single message field before chunking is required (4 MiB).
	DefaultMaxChunkBytes uint32 = 4 * 1024 * 1024
)

// NegotiateChunkSize returns the effective maximum chunk size given the
// adapter's declared limit and the host's own limit.
//
// Either value may be zero, which means "use the protocol default (4 MiB)".
// The effective limit is min(non-zero values, DefaultMaxChunkBytes).
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

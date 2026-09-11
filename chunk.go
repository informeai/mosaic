package mosaic

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math/rand"

	"github.com/restic/chunker"
)

// chunkPolynomial is derived once from a fixed seed instead of randomly at
// runtime: identical content must always split into identical chunk
// boundaries across separate encode runs (today, next week, on a
// different machine), otherwise two copies of the same bytes would hash
// to different chunks and deduplication would never kick in.
var chunkPolynomial = mustPolynomial()

func mustPolynomial() chunker.Pol {
	seed := sha256.Sum256([]byte("mosaic-chunk-polynomial-v1"))
	// A seeded PRNG gives DerivePolynomial an effectively endless, varied
	// (but fully reproducible) stream of candidate bytes to search through.
	source := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(seed[:8]))))
	pol, err := chunker.DerivePolynomial(source)
	if err != nil {
		panic("mosaic: could not derive chunk polynomial: " + err.Error())
	}
	return pol
}

// ChunkSize configures content-defined chunking's boundaries: chunks land
// between Min and Max bytes, averaging roughly 2^AvgBits bytes.
//
// Every chunk costs something to process wherever it's stored (a row to
// read, a decompress call, a hash to check) independent of its size — so
// the right size is a trade-off between two things pulling opposite ways:
// smaller chunks give finer-grained deduplication (an edit only reshuffles
// the chunks right around it) but mean more of that per-chunk overhead for
// a given file size; larger chunks mean less overhead but coarser dedup.
// There's no single right answer — it depends on what the chunks are for.
type ChunkSize struct {
	Min     int
	Max     int
	AvgBits int
}

// DefaultChunkSize is tuned small on purpose: it's calibrated for
// transport media with tight per-unit budgets (a QR code, a printed
// bitmap tile) — not the multi-megabyte chunks chunker defaults to for
// backup tools, and not sized for a store like SQLite that has no such
// per-unit constraint at all (see sqlitestore, which uses a larger
// ChunkSize for exactly that reason).
var DefaultChunkSize = ChunkSize{Min: 512, Max: 4096, AvgBits: 11} // ~2KiB average

// splitChunks reads r and calls fn once per content-defined chunk, in
// order. Boundaries are determined by the content itself (a rolling
// checksum), not by byte offset — so inserting or removing bytes near the
// start of a file only reshuffles the chunks around that edit, leaving
// everything else, and its hash, unchanged.
func splitChunks(r io.Reader, size ChunkSize, fn func(data []byte) error) error {
	c := chunker.New(r, chunkPolynomial,
		chunker.WithBoundaries(uint(size.Min), uint(size.Max)),
		chunker.WithAverageBits(size.AvgBits),
	)
	buf := make([]byte, size.Max)
	for {
		chunk, err := c.Next(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Next reuses buf on each call, so copy out before it's overwritten.
		data := make([]byte, len(chunk.Data))
		copy(data, chunk.Data)
		if err := fn(data); err != nil {
			return err
		}
	}
}

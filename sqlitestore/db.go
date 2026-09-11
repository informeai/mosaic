// Package sqlitestore is a SQLite-backed manifest/chunk store for mosaic.
// It is kept separate from the root mosaic package so that importing
// mosaic's core chunking/encode/decode logic never pulls in the SQLite
// driver (and its cgo-free but still sizeable dependency tree) — only code
// that actually needs a shared, id-addressed store imports this package.
package sqlitestore

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"mosaic"
)

// DB is a SQLite-backed content store: chunks keyed by hash, plus one
// manifest row per distinct file content — keyed by that content's own
// file hash — and the ordered chunk references that reconstruct it. This
// is what lets an API hand back a short identifier instead of the chunk
// data itself: reconstruction later only needs that identifier and this
// database, nothing carried by the caller in between.
type DB struct {
	sql *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS chunks (
	hash TEXT PRIMARY KEY,
	data BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS manifests (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS manifest_chunks (
	manifest_id TEXT NOT NULL REFERENCES manifests(id),
	position    INTEGER NOT NULL,
	chunk_hash  TEXT NOT NULL,
	PRIMARY KEY (manifest_id, position)
);

CREATE INDEX IF NOT EXISTS idx_manifest_chunks_manifest ON manifest_chunks(manifest_id);
`

// OpenDB opens (creating and migrating if needed) a SQLite database at path.
func OpenDB(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite serializes writers anyway; keeping the pool at one connection
	// avoids "database is locked" surprises from concurrent handlers.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("mosaic/sqlitestore: migrating db: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

// ManifestSummary is the lightweight listing shape: enough to show what a
// manifest is (name, size, when it was created) without pulling every
// chunk reference.
type ManifestSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

func scanManifestSummary(scan func(dest ...any) error) (*ManifestSummary, error) {
	var m ManifestSummary
	var createdAt string
	if err := scan(&m.ID, &m.Name, &m.Size, &createdAt); err != nil {
		return nil, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = parsed
	return &m, nil
}

// insertBatchSize is how many rows accumulate before a batch is flushed as
// one multi-row INSERT. Kept comfortably under SQLite's variable-count
// limit (the tightest builds cap at 999) even for manifest_chunks' 3
// params/row: 300 rows * 3 = 900.
const insertBatchSize = 300

// chunkSize is deliberately much larger than mosaic.DefaultChunkSize: that
// default is calibrated for a per-unit transport budget (a QR code) this
// store doesn't have. Every chunk costs a SQL row, a compress/decompress
// call, and a hash check regardless of its size — profiling a 1GB
// reconstruction showed that cost is dominated by how many chunks there
// are (~488k at the ~2KiB default), not by bytes processed. Averaging
// ~64KiB instead cuts the chunk count, and every one of those per-chunk
// costs, by roughly the same ~32x — at the price of coarser
// deduplication (an edit now reshuffles a larger neighborhood).
var chunkSize = mosaic.ChunkSize{Min: 16 << 10, Max: 128 << 10, AvgBits: 16} // ~64KiB average

// execBatch runs one multi-row "insertPrefix VALUES (?,...),(?,...),..."
// built from rows, each supplying exactly placeholdersPerRow args. This is
// what keeps writing a large file's chunks (or a large manifest's chunk
// references) from turning into one round-trip per row: SQLite parses and
// plans the statement once per batch instead of once per row.
func execBatch(tx *sql.Tx, insertPrefix string, placeholdersPerRow int, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", placeholdersPerRow), ",") + ")"

	var sb strings.Builder
	sb.WriteString(insertPrefix)
	args := make([]any, 0, len(rows)*placeholdersPerRow)
	for i, row := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(placeholder)
		args = append(args, row...)
	}
	_, err := tx.Exec(sb.String(), args...)
	return err
}

// EncodeToDB streams src into db under name, returning the manifest id
// (the content's own hash) a caller uses later to reconstruct it.
// Encoding identical content twice is a no-op beyond the first time — same
// id comes back, nothing rewritten, the original name wins.
//
// src is read exactly once, chunk by chunk: memory use stays bounded by
// chunk size and the (much smaller) set of chunk hashes seen so far, not
// by the size of src, so a file far larger than available RAM encodes
// fine. Because the file's own hash (the id) can only be known once every
// byte has been read, the "is this content already stored" check happens
// after chunking rather than before — a re-upload of identical content
// still gets chunked, but nothing new is written (INSERT OR IGNORE on the
// chunks already makes that a no-op, and the manifest/reference rows are
// skipped entirely once the id is known to already exist).
func EncodeToDB(db *DB, name string, src io.Reader) (*ManifestSummary, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var pending [][]any
	flushChunks := func() error {
		if err := execBatch(tx, `INSERT OR IGNORE INTO chunks (hash, data) VALUES `, 2, pending); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	fileHash, chunkHashes, size, err := mosaic.ChunkAndCompressReader(src, chunkSize, func(h mosaic.Hash, compressed []byte) error {
		pending = append(pending, []any{h.String(), compressed})
		if len(pending) >= insertBatchSize {
			return flushChunks()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := flushChunks(); err != nil {
		return nil, err
	}

	id := fileHash.String()

	existing, err := scanManifestSummary(tx.QueryRow(
		`SELECT id, name, size, created_at FROM manifests WHERE id = ?`, id).Scan)
	if err == nil {
		// Identical content already has a manifest — the chunk rows this
		// call just (re-)inserted are harmless no-ops (INSERT OR IGNORE);
		// commit them anyway and hand back the original manifest.
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	createdAt := time.Now().UTC()
	if _, err := tx.Exec(`INSERT INTO manifests (id, name, size, created_at) VALUES (?, ?, ?, ?)`,
		id, name, size, createdAt.Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}

	var refs [][]any
	flushRefs := func() error {
		if err := execBatch(tx, `INSERT INTO manifest_chunks (manifest_id, position, chunk_hash) VALUES `, 3, refs); err != nil {
			return err
		}
		refs = refs[:0]
		return nil
	}
	for i, h := range chunkHashes {
		refs = append(refs, []any{id, i, h.String()})
		if len(refs) >= insertBatchSize {
			if err := flushRefs(); err != nil {
				return nil, err
			}
		}
	}
	if err := flushRefs(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &ManifestSummary{ID: id, Name: name, Size: size, CreatedAt: createdAt}, nil
}

// ListManifests returns every manifest in db, most recently created first.
func ListManifests(db *DB) ([]ManifestSummary, error) {
	rows, err := db.sql.Query(`SELECT id, name, size, created_at FROM manifests ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ManifestSummary
	for rows.Next() {
		m, err := scanManifestSummary(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// tempFileReader wraps a temp file so Close both closes and removes it —
// the file exists only to spool one DecodeFromDB call's verified output
// between "fully checked" and "the caller has read it all".
type tempFileReader struct {
	*os.File
	path string
}

func (t *tempFileReader) Close() error {
	closeErr := t.File.Close()
	if err := os.Remove(t.path); err != nil && closeErr == nil {
		closeErr = err
	}
	return closeErr
}

// DecodeFromDB validates every chunk referenced by manifest id and
// reassembles them in order into a temporary file, verifying the result
// against id itself (which is the file's hash) — exactly the checks the
// in-memory version used to make, just against a spooled file instead of
// a byte slice, so memory use stays bounded by chunk size rather than
// file size. Nothing is readable from the returned io.ReadCloser until
// every check has already passed: a corrupted chunk, a manifest
// referencing a missing chunk, or a mismatched final hash all fail before
// DecodeFromDB returns, not partway through the caller reading it.
//
// The caller must Close the returned ReadCloser once done reading it —
// that also removes the backing temp file.
func DecodeFromDB(db *DB, id string) (*ManifestSummary, io.ReadCloser, error) {
	m, err := scanManifestSummary(db.sql.QueryRow(
		`SELECT id, name, size, created_at FROM manifests WHERE id = ?`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("mosaic/sqlitestore: no manifest with id %q", id)
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := db.sql.Query(`
		SELECT mc.chunk_hash, c.data
		FROM manifest_chunks mc
		LEFT JOIN chunks c ON c.hash = mc.chunk_hash
		WHERE mc.manifest_id = ?
		ORDER BY mc.position`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, nil, err
	}
	defer dec.Close()

	tmp, err := os.CreateTemp("", "mosaic-decode-*")
	if err != nil {
		return nil, nil, err
	}
	tf := &tempFileReader{File: tmp, path: tmp.Name()}
	ok := false
	defer func() {
		if !ok {
			tf.Close()
		}
	}()

	hasher := sha256.New()
	for rows.Next() {
		var hashHex string
		var compressed []byte
		if err := rows.Scan(&hashHex, &compressed); err != nil {
			return nil, nil, err
		}
		h, err := mosaic.ParseHash(hashHex)
		if err != nil {
			return nil, nil, err
		}
		if compressed == nil {
			return nil, nil, fmt.Errorf("mosaic/sqlitestore: manifest %q references missing chunk %s", id, h)
		}

		raw, err := dec.DecodeAll(compressed, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("mosaic/sqlitestore: chunk %s failed to decompress: %w", h, err)
		}
		if mosaic.HashBytes(raw) != h {
			return nil, nil, fmt.Errorf("mosaic/sqlitestore: chunk %s failed its integrity check (corrupted or tampered)", h)
		}
		if _, err := tmp.Write(raw); err != nil {
			return nil, nil, err
		}
		hasher.Write(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	fileHash, err := mosaic.ParseHash(id)
	if err != nil {
		return nil, nil, fmt.Errorf("mosaic/sqlitestore: manifest id %q is not a valid hash", id)
	}
	var sum mosaic.Hash
	copy(sum[:], hasher.Sum(nil))
	if sum != fileHash {
		return nil, nil, fmt.Errorf("mosaic/sqlitestore: reconstructed file does not match manifest id — corrupted or tampered")
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}

	ok = true
	return m, tf, nil
}

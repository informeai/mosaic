package mosaic

import (
	"bytes"
	"database/sql"
	"fmt"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
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
		return nil, fmt.Errorf("mosaic: migrating db: %w", err)
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

// EncodeToDB chunks raw and stores it in db under name, returning the
// manifest id (the content's own hash) a caller uses later to reconstruct
// it. Encoding identical content twice is a no-op beyond the first time —
// same id comes back, nothing rewritten, the original name wins.
func EncodeToDB(db *DB, name string, raw []byte) (*ManifestSummary, error) {
	id := hashBytes(raw).String()

	existing, err := scanManifestSummary(db.sql.QueryRow(
		`SELECT id, name, size, created_at FROM manifests WHERE id = ?`, id).Scan)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	chunkHashes, units, err := chunkAndCompress(raw)
	if err != nil {
		return nil, err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for h, compressed := range units {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO chunks (hash, data) VALUES (?, ?)`,
			h.String(), compressed); err != nil {
			return nil, err
		}
	}

	createdAt := time.Now().UTC()
	if _, err := tx.Exec(`INSERT INTO manifests (id, name, size, created_at) VALUES (?, ?, ?, ?)`,
		id, name, len(raw), createdAt.Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}

	insertRef, err := tx.Prepare(`INSERT INTO manifest_chunks (manifest_id, position, chunk_hash) VALUES (?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insertRef.Close()
	for i, h := range chunkHashes {
		if _, err := insertRef.Exec(id, i, h.String()); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &ManifestSummary{ID: id, Name: name, Size: int64(len(raw)), CreatedAt: createdAt}, nil
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

// DecodeFromDB validates every chunk referenced by manifest id, reassembles
// them in order, checks the result against id itself (which is the file's
// hash), and returns the reconstructed bytes alongside the manifest.
func DecodeFromDB(db *DB, id string) (*ManifestSummary, []byte, error) {
	m, err := scanManifestSummary(db.sql.QueryRow(
		`SELECT id, name, size, created_at FROM manifests WHERE id = ?`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("mosaic: no manifest with id %q", id)
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

	var buf bytes.Buffer
	for rows.Next() {
		var hashHex string
		var compressed []byte
		if err := rows.Scan(&hashHex, &compressed); err != nil {
			return nil, nil, err
		}
		h, err := ParseHash(hashHex)
		if err != nil {
			return nil, nil, err
		}
		if compressed == nil {
			return nil, nil, fmt.Errorf("mosaic: manifest %q references missing chunk %s", id, h)
		}

		raw, err := dec.DecodeAll(compressed, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("mosaic: chunk %s failed to decompress: %w", h, err)
		}
		if hashBytes(raw) != h {
			return nil, nil, fmt.Errorf("mosaic: chunk %s failed its integrity check (corrupted or tampered)", h)
		}
		buf.Write(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	fileHash, err := ParseHash(id)
	if err != nil {
		return nil, nil, fmt.Errorf("mosaic: manifest id %q is not a valid hash", id)
	}
	if hashBytes(buf.Bytes()) != fileHash {
		return nil, nil, fmt.Errorf("mosaic: reconstructed file does not match manifest id — corrupted or tampered")
	}

	return m, buf.Bytes(), nil
}

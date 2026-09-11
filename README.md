# mosaic

Content-addressed file reconstruction: split a file into content-defined
chunks, hash each one, and describe the file as an ordered manifest of
those hashes. Reconstruction just needs the manifest and the chunks it
references — in any order, from any subset that later completes, and
skipping anything already known.

The reconstruction logic is deliberately decoupled from any transport.
The "pattern" produced by encoding can be a directory of files, a
self-contained JSON document, rows in a SQLite database, or a sequence of
QR-code images — any of these layer on top without touching the core
logic.

## Why content-defined chunking

Chunk boundaries are found by a rolling checksum over the content itself,
not by fixed byte offsets. Inserting or deleting bytes near the start of a
file only reshuffles the chunks around that edit — everything else, and
its hash, stays identical. That's what makes deduplication actually work
across versions of the same file, not just inside a single one.

## Properties

- **Deduplication** — identical chunks, whether repeated inside one file
  or shared across two different files, are only ever stored/transported
  once.
- **Resumable** — decoding from a partial set of chunks reports progress
  instead of failing; feeding it the rest later picks up where it left
  off.
- **Order-independent** — every chunk verifies itself against its own
  hash, so chunks can arrive in any order.
- **Tamper-evident** — a corrupted or altered chunk is rejected before
  assembly; the fully reconstructed file is checked against the
  manifest's file hash too.

## Storage/transport shapes

| Shape | Produced/read by | Use it when |
|---|---|---|
| Pattern directory (`manifest.json` + `chunks/*.unit`) | `Encode` / `Decode` | Files are the transport (disk, a zip) |
| Bundle (single JSON file, chunks embedded) | `EncodeBundle` / `DecodeBundle` | One self-contained document is more convenient than a directory |
| SQLite database | `EncodeToDB` / `DecodeFromDB` / `ListManifests` | A CLI and an API need to share the same store, addressed by a short id |
| QR-code PNGs | `EncodeToQR` / `DecodeFromQR` (`mosaic/qrcode`) | Nothing exists between two devices but a camera — an air-gapped transfer, a printed sheet |

The first three share the same chunking, hashing, and compression
underneath (`ChunkAndCompress` in `encode.go`) — they're just different
ways of carrying the same chunks around, though not necessarily at the
same `ChunkSize`: the directory/Bundle shapes use `DefaultChunkSize`
(~2KiB average, calibrated for a per-unit transport budget like a QR
code), while `sqlitestore` uses a larger one (~64KiB average) since a
database row has no such constraint and every chunk costs a SQL row, a
compress/decompress call, and a hash check regardless of its size —
profiling a 1GB reconstruction found that cost dominated by chunk count,
not bytes, so fewer/bigger chunks cut it proportionally (at the price of
coarser deduplication: an edit reshuffles a larger neighborhood). The QR
shape works one level up:
it re-frames a whole Bundle's JSON into small, fixed-size, self-describing
pieces sized for reliable scanning, independent of the file's own chunk
sizes (see `qrcode/qrcode.go` for why).

## Package layout

The core (`chunk.go`, `encode.go`, `decode.go`, `manifest.go`, `bundle.go`,
`store.go`, package `mosaic`) has no dependency on SQLite or on anything
CLI/HTTP-specific — `go get`-ing it into another project pulls in only
`klauspost/compress` and `restic/chunker`.

The SQLite-backed store and the QR-code renderer/reader each live in
their own subpackage — `mosaic/sqlitestore` and `mosaic/qrcode` — so that
only code that actually needs a shared id-addressed store, or actually
renders/reads QR codes, pays for those dependencies:

```go
import "mosaic"              // Encode, Decode, EncodeBundle, DecodeBundle, Store
import "mosaic/sqlitestore"  // OpenDB, EncodeToDB, DecodeFromDB, ListManifests
import "mosaic/qrcode"       // EncodeToQR, DecodeFromQR
```

`cmd/mosaic` and `cmd/mosaicd` are themselves just callers of these
packages — no reconstruction logic of their own.

## Install

```
go build ./cmd/mosaic    # CLI
go build ./cmd/mosaicd   # HTTP API
```

Requires Go 1.22+ (uses `net/http`'s method/wildcard routing patterns).

## CLI

```
mosaic encode [--bundle] <input-file> <output>
mosaic encode --db <db-path> <input-file>
mosaic encode --qr <input-file> <output-dir>

mosaic decode [--bundle] <input> <output-file> [store-dir]
mosaic decode --db <db-path> <manifest-id> <output-file>
mosaic decode --qr <qr-dir> <output-file> [store-dir]

mosaic list --db <db-path>
```

**Directory mode** (default) — the pattern is a folder you can copy,
zip, or eventually print:

```
mosaic encode photo.png ./pattern
mosaic decode ./pattern photo-reconstructed.png
```

**Bundle mode** — one JSON file instead of a directory:

```
mosaic encode --bundle photo.png bundle.json
mosaic decode --bundle bundle.json photo-reconstructed.png
```

**Database mode** — encode prints a manifest id (to stdout, so scripts
can capture it); decode reconstructs from that id:

```
id=$(mosaic encode --db mosaic.db photo.png)
mosaic list --db mosaic.db
mosaic decode --db mosaic.db "$id" photo-reconstructed.png
```

Encoding identical content twice returns the same id — nothing is
duplicated in the database.

**QR mode** — a sequence of QR-code PNGs, meant for a transfer with no
network between the two sides at all: display them one after another (or
print them) for a camera to scan, and scan them back in any order, across
as many passes as it takes:

```
mosaic encode --qr photo.png ./qr
mosaic decode --qr ./qr photo-reconstructed.png
```

A single QR code tops out around ~2KB of binary data even at a forgiving
error-correction level, so frames here are sized independently of the
file's own chunk boundaries — see `mosaic/qrcode`'s package doc for why.
Decoding from a partial set of frames reports `N/M frames captured`
instead of failing outright, same as the default directory mode does for
chunks.

**qr-url** — unrelated to reconstruction: renders a single QR-code PNG
that just encodes whatever text you give it, no framing or chunking. For
when `mosaicd` is already reachable on the network and the QR only needs
to point at it (see the HTTP API section below) rather than carry the
file itself:

```
mosaic qr-url http://192.168.1.50:8085/manifests/<id> download.png
```

## HTTP API (`mosaicd`)

```
mosaicd -addr :8085 -db mosaic.db -max-upload-mb 8192
```

`-max-upload-mb` caps the request body on every endpoint (default 8192,
i.e. 8GiB) — it's a safety net against a runaway client, not a memory
bound: `/manifests` streams the upload straight through chunking into
SQLite, so raising it doesn't cost more RAM, just allows bigger files.

**Stateless** (nothing kept server-side beyond the request):

- `POST /encode` — multipart field `file` → a zip of the pattern
  directory. Add `?format=json` to get a self-contained Bundle instead.
- `POST /decode` — a Bundle JSON body → the reconstructed file's raw
  bytes.

These two still hold their payload in memory (a Bundle is inherently
self-contained), so treat `-max-upload-mb` more conservatively if they see
real traffic.

**Database-backed** (shares `mosaic.db` with the CLI's `--db` mode; both
endpoints stream — memory use stays flat regardless of file size):

- `POST /manifests` — multipart field `file` → `{id, name, size, created_at}`.
  The upload is chunked and written to SQLite as it's received, in
  batched multi-row inserts, never fully materialized in the server's
  memory.
- `GET /manifests` — JSON array of every stored manifest.
- `GET /manifests/{id}` — validates every referenced chunk, reconstructs
  into a temporary file (verified in full before any byte reaches the
  response), and streams it back.

```
curl -F file=@photo.png http://localhost:8085/manifests
curl http://localhost:8085/manifests
curl http://localhost:8085/manifests/<id> -o photo-reconstructed.png
```

Sharing `GET /manifests/{id}` as a QR code (`mosaic qr-url`) is a fast
local-network alternative to `--qr`: the file's content never crosses the
optical channel, only a link to where it already lives on the server —
reconstruction is a hash lookup + zstd decompress away, not a re-chunking,
so it stays fast (milliseconds, not the many QR frames `--qr` needs for
anything beyond a tiny file). It trades the "no network at all" property
of `--qr`/`--db`-less transfer for speed and a much smaller code.

## Tests

```
go test ./...
```

Covers round-trip reconstruction, resuming from a partial pattern,
deduplication across two files sharing content, the database's
idempotent-encode behavior, and (in `mosaic/qrcode`) round-tripping
through actual rendered/decoded QR-code images, including resuming from
only half the frames.

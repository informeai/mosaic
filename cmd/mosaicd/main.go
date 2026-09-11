// Command mosaicd exposes mosaic's encode/decode logic over HTTP.
//
// Two independent surfaces live here:
//
//   - /encode and /decode are stateless: a client gets back everything
//     needed to reconstruct (a zip, or a self-contained JSON Bundle) and
//     carries it away — nothing is kept server-side.
//   - /manifests is backed by a shared SQLite database: POST stores the
//     file server-side and returns just an id, GET /manifests/{id}
//     validates and reconstructs from that id, GET /manifests lists
//     what's stored. The same database file is meant to be shared with
//     the mosaic CLI's --db mode.
package main

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"mosaic"
	"mosaic/sqlitestore"
)

// maxUploadBytes caps the total size of any request body, across every
// endpoint — set from -max-upload-mb at startup. It exists as a safety
// net against a runaway/malicious client, not as a memory bound: /manifests
// (the DB-backed endpoints) streams the upload straight through chunking
// into SQLite, so raising this doesn't cost more RAM, just allows bigger
// files. /encode and /decode (the stateless, Bundle/zip endpoints) still
// hold their payload in memory, so treat this cap more conservatively if
// those see traffic too.
var maxUploadBytes int64 = 8 << 30 // 8GiB default

// multipartMemoryThreshold bounds how much of a multipart upload
// mime/multipart buffers in RAM before it spills the rest to its own temp
// file — kept small and independent of maxUploadBytes so a large upload
// never sits fully in memory just because the request-size cap is large.
const multipartMemoryThreshold = 32 << 20 // 32MiB

func main() {
	addr := flag.String("addr", ":8085", "listen address")
	dbPath := flag.String("db", "mosaic.db", "path to the shared SQLite database backing /manifests")
	maxUploadMB := flag.Int64("max-upload-mb", 8192, "maximum request body size, in MiB, for every endpoint")
	flag.Parse()
	maxUploadBytes = *maxUploadMB << 20

	db, err := sqlitestore.OpenDB(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer db.Close()
	srv := &server{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /encode", handleEncode)
	mux.HandleFunc("POST /decode", handleDecode)
	mux.HandleFunc("POST /manifests", srv.createManifest)
	mux.HandleFunc("GET /manifests", srv.listManifests)
	mux.HandleFunc("GET /manifests/{id}", srv.getManifest)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("mosaicd listening on %s (db: %s)", *addr, *dbPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type server struct {
	db *sqlitestore.DB
}

// createManifest: POST /manifests, multipart field "file" -> stores the
// file's chunks and manifest in the database, returns
// {id, name, size, created_at} as JSON. The id is the file's own content
// hash, so uploading identical content again just returns the same id.
func (s *server) createManifest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	// A small maxMemory here is deliberate: above it, mime/multipart spills
	// the upload to its own temp file instead of buffering it in RAM, so a
	// multi-GB file never sits fully in memory just to get parsed out of
	// the multipart body.
	if err := r.ParseMultipartForm(multipartMemoryThreshold); err != nil {
		http.Error(w, "file too large or malformed upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `expected a multipart field named "file"`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// file streams straight into chunking/hashing/compression and on into
	// the database — nothing here reads it into a single []byte first.
	summary, err := sqlitestore.EncodeToDB(s.db, header.Filename, file)
	if err != nil {
		http.Error(w, "encode failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(summary); err != nil {
		log.Printf("json encode failed after headers were sent: %v", err)
	}
}

// listManifests: GET /manifests -> JSON array of {id, name, size, created_at}.
func (s *server) listManifests(w http.ResponseWriter, r *http.Request) {
	list, err := sqlitestore.ListManifests(s.db)
	if err != nil {
		log.Printf("list manifests failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(list); err != nil {
		log.Printf("json encode failed after headers were sent: %v", err)
	}
}

// getManifest: GET /manifests/{id} -> validates every chunk the manifest
// references, reconstructs, and returns the raw file bytes.
func (s *server) getManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// DecodeFromDB has already verified every chunk and the whole file's
	// hash by the time it returns — rc is a spooled temp file, not a
	// buffer, so reconstructing a multi-GB file doesn't hold it all in
	// RAM. Headers go out before any of it is copied to w.
	summary, rc, err := sqlitestore.DecodeFromDB(s.db, id)
	if err != nil {
		http.Error(w, "decode failed: "+err.Error(), http.StatusNotFound)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, summary.Name))
	w.Header().Set("X-Mosaic-Manifest-Id", summary.ID)
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("streaming reconstructed file failed after headers were sent: %v", err)
	}
}

// handleEncode: POST /encode, multipart field "file" ->
//   - by default, a zip of manifest.json + chunks/*.unit, ready to unpack
//     straight into a pattern directory that Decode (or a QR-rendering
//     step) can consume
//   - with ?format=json, a single self-contained Bundle (manifest fields
//     plus every chunk's bytes embedded) — everything /decode needs is in
//     that one JSON document, nothing else to fetch
//
// Stateless: nothing here touches the database backing /manifests.
func handleEncode(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryThreshold); err != nil {
		http.Error(w, "file too large or malformed upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `expected a multipart field named "file"`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	workDir, err := os.MkdirTemp("", "mosaicd-*")
	if err != nil {
		log.Printf("mkdir temp failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(workDir)

	inputPath := filepath.Join(workDir, filepath.Base(header.Filename))
	if err := saveUpload(inputPath, file); err != nil {
		log.Printf("saving upload failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("format") == "json" {
		b, err := mosaic.EncodeBundle(inputPath)
		if err != nil {
			http.Error(w, "encode failed: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mosaic-Unique-Chunks", fmt.Sprintf("%d", len(b.UniqueChunks())))
		w.Header().Set("X-Mosaic-File-Hash", b.FileHash.String())
		if err := json.NewEncoder(w).Encode(b); err != nil {
			log.Printf("json encode failed after headers were sent: %v", err)
		}
		return
	}

	patternDir := filepath.Join(workDir, "pattern")
	m, err := mosaic.Encode(inputPath, patternDir)
	if err != nil {
		http.Error(w, "encode failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.mosaic.zip"`, m.Name))
	w.Header().Set("X-Mosaic-Unique-Chunks", fmt.Sprintf("%d", len(m.UniqueChunks())))
	w.Header().Set("X-Mosaic-File-Hash", m.FileHash.String())
	if err := zipDir(w, patternDir); err != nil {
		log.Printf("zip stream failed after headers were sent: %v", err)
	}
}

// handleDecode: POST /decode, body = a Bundle as returned by
// POST /encode?format=json -> the reconstructed file's raw bytes.
// Stateless: uses a throwaway chunk cache, not the /manifests database.
func handleDecode(w http.ResponseWriter, r *http.Request) {
	var b mosaic.Bundle
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err := dec.Decode(&b); err != nil {
		http.Error(w, "invalid bundle JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	storeDir, err := os.MkdirTemp("", "mosaicd-decode-*")
	if err != nil {
		log.Printf("mkdir temp failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(storeDir)

	store, err := mosaic.OpenStore(storeDir)
	if err != nil {
		log.Printf("open store failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data, err := mosaic.DecodeBundle(&b, store)
	if err != nil {
		http.Error(w, "decode failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, b.Name))
	w.Header().Set("X-Mosaic-File-Hash", b.FileHash.String())
	w.Write(data)
}

func saveUpload(dstPath string, src io.Reader) error {
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

// zipDir streams dir's contents into w as a zip, with paths relative to
// dir, so the result unpacks straight into manifest.json + chunks/.
func zipDir(w io.Writer, dir string) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entry, err := zw.Create(rel)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(entry, src)
		return err
	})
}

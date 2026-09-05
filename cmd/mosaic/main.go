// Command mosaic encodes a file into a reconstruction pattern and decodes
// that pattern back into the original file — the transport in between
// (QR codes, a printed bitmap, an HTTP call) is deliberately out of scope
// here. Three pattern shapes are supported: a directory (manifest.json +
// chunks/*.unit) by default, a single self-contained Bundle JSON file
// with --bundle, or a shared SQLite database with --db.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"mosaic"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "encode":
		runEncodeCmd(os.Args[2:])
	case "decode":
		runDecodeCmd(os.Args[2:])
	case "list":
		runListCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mosaic encode [--bundle] <input-file> <output>
  mosaic encode --db <db-path> <input-file>
  mosaic decode [--bundle] <input> <output-file> [store-dir]
  mosaic decode --db <db-path> <manifest-id> <output-file>
  mosaic list --db <db-path>

Without flags, encode writes a pattern directory:
  <output>/manifest.json + <output>/chunks/*.unit
and decode reads that same shape from <input>, any subset in any
order — it's resumable — caching reconstructed chunks in [store-dir]
(default: ./mosaic-store) and writing <output-file> once complete.

With --bundle, encode writes a single self-contained JSON file to
<output> (manifest + every chunk's bytes embedded), and decode reads
that same file from <input>.

With --db, encode and decode share a SQLite database instead of any
file-based pattern: encode prints the manifest id it was stored under
(to stdout, so scripts can capture it), and decode reconstructs from
that id. list shows every manifest in the database (id, name, size).`)
	os.Exit(2)
}

func runEncodeCmd(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	bundle := fs.Bool("bundle", false, "write a single self-contained Bundle JSON file instead of a pattern directory")
	dbPath := fs.String("db", "", "encode into a shared SQLite database instead of files; prints the manifest id")
	fs.Parse(args)

	rest := fs.Args()

	if *dbPath != "" {
		if *bundle {
			fmt.Fprintln(os.Stderr, "--bundle and --db are mutually exclusive")
			os.Exit(2)
		}
		if len(rest) != 1 {
			usage()
		}
		runEncodeDB(*dbPath, rest[0])
		return
	}

	if len(rest) != 2 {
		usage()
	}
	input, output := rest[0], rest[1]
	if *bundle {
		runEncodeBundle(input, output)
		return
	}
	runEncode(input, output)
}

func runDecodeCmd(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	bundle := fs.Bool("bundle", false, "read a Bundle JSON file instead of a pattern directory")
	dbPath := fs.String("db", "", "reconstruct from a shared SQLite database using a manifest id")
	fs.Parse(args)

	rest := fs.Args()

	if *dbPath != "" {
		if *bundle {
			fmt.Fprintln(os.Stderr, "--bundle and --db are mutually exclusive")
			os.Exit(2)
		}
		if len(rest) != 2 {
			usage()
		}
		runDecodeDB(*dbPath, rest[0], rest[1])
		return
	}

	if len(rest) < 2 || len(rest) > 3 {
		usage()
	}
	source, output := rest[0], rest[1]
	storeDir := "mosaic-store"
	if len(rest) == 3 {
		storeDir = rest[2]
	}
	if *bundle {
		runDecodeBundle(source, output, storeDir)
		return
	}
	runDecode(source, output, storeDir)
}

func runListCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", "mosaic.db", "path to the shared SQLite database")
	fs.Parse(args)

	db, err := mosaic.OpenDB(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db failed:", err)
		os.Exit(1)
	}
	defer db.Close()

	list, err := mosaic.ListManifests(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list failed:", err)
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Println("(no manifests)")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSIZE\tCREATED_AT")
	for _, m := range list {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", m.ID, m.Name, m.Size, m.CreatedAt.Format(time.RFC3339))
	}
	w.Flush()
}

func runEncode(input, patternDir string) {
	m, err := mosaic.Encode(input, patternDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode failed:", err)
		os.Exit(1)
	}
	fmt.Printf("encoded %q: %d bytes -> %d chunk refs (%d unique units) in %s\n",
		m.Name, m.Size, len(m.ChunkHashes), len(m.UniqueChunks()), patternDir)
}

func runEncodeBundle(input, outputFile string) {
	b, err := mosaic.EncodeBundle(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode failed:", err)
		os.Exit(1)
	}

	f, err := os.Create(outputFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Printf("encoded %q: %d bytes -> %d chunk refs (%d unique units) in %s (bundle)\n",
		b.Name, b.Size, len(b.ChunkHashes), len(b.UniqueChunks()), outputFile)
}

func runEncodeDB(dbPath, input string) {
	db, err := mosaic.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db failed:", err)
		os.Exit(1)
	}
	defer db.Close()

	raw, err := os.ReadFile(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read failed:", err)
		os.Exit(1)
	}

	summary, err := mosaic.EncodeToDB(db, filepath.Base(input), raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode failed:", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "encoded %q: %d bytes, manifest id %s\n", summary.Name, summary.Size, summary.ID)
	fmt.Println(summary.ID) // stdout carries just the id, so scripts can do: id=$(mosaic encode --db ...)
}

func runDecode(patternDir, output, storeDir string) {
	store, err := mosaic.OpenStore(storeDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store open failed:", err)
		os.Exit(1)
	}

	m, data, err := mosaic.Decode(patternDir, store)
	if err != nil {
		if m != nil {
			have, total := mosaic.Progress(m, store)
			fmt.Printf("progress: %d/%d unique chunks captured\n", have, total)
		}
		fmt.Fprintln(os.Stderr, "decode incomplete:", err)
		os.Exit(1)
	}

	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Printf("reconstructed %q -> %s (%d bytes, integrity verified)\n", m.Name, output, len(data))
}

func runDecodeBundle(bundlePath, output, storeDir string) {
	f, err := os.Open(bundlePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open failed:", err)
		os.Exit(1)
	}
	defer f.Close()

	var b mosaic.Bundle
	if err := json.NewDecoder(f).Decode(&b); err != nil {
		fmt.Fprintln(os.Stderr, "invalid bundle:", err)
		os.Exit(1)
	}

	store, err := mosaic.OpenStore(storeDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store open failed:", err)
		os.Exit(1)
	}

	data, err := mosaic.DecodeBundle(&b, store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode failed:", err)
		os.Exit(1)
	}

	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Printf("reconstructed %q -> %s (%d bytes, integrity verified)\n", b.Name, output, len(data))
}

func runDecodeDB(dbPath, id, output string) {
	db, err := mosaic.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db failed:", err)
		os.Exit(1)
	}
	defer db.Close()

	summary, data, err := mosaic.DecodeFromDB(db, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode failed:", err)
		os.Exit(1)
	}

	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Printf("reconstructed %q -> %s (%d bytes, integrity verified)\n", summary.Name, output, len(data))
}

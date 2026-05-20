# Go Advanced TAR Library (with Ratarmount-compatible Indexing)

[![Go Reference](https://pkg.go.dev/badge/github.com/unxed/tar.svg)](https://pkg.go.dev/github.com/unxed/tar)

This library is a highly optimized and advanced drop-in replacement for the Go standard library `archive/tar`.

It integrates high-speed compression, parallel extraction, and `ratarmount`-compatible SQLite indexing to enable instant random access within massive archives.

## Key Features

* **Drop-in Compatibility:** 100% compatible with the `archive/tar` API. You can seamlessly replace your existing `archive/tar` imports with `github.com/unxed/tar`.
* **High Performance:** Uses `klauspost/compress` for highly-parallelized `ZStandard` and optimized `Gzip` compression streams.
* **Broad Compression Support:** Built-in automatic format detection (magic bytes) for `GZIP`, `BZIP2`, `XZ`, and `ZSTD`.
* **Parallel Extraction:** Reads TAR sequentially but delegates filesystem writes, file creations, and metadata restoration (`chmod`/`chown`) to a parallel worker pool.
* **In-place Updates (Updater):** Truncates existing TAR EOF zero blocks to allow appending new files without full archive rewrite.
* **Ratarmount-compatible Indexing & Random Access (`fs.FS`):**
  * Indexes `.tar` or compressed `.tar.*` archives on the fly into an SQLite database with a schema identical to `ratarmount`.
  * Exposes the archive as a standard Go `fs.FS` interface.
  * Facilitates **O(1) random-access** file reading for uncompressed `.tar` archives via `io.SectionReader`.
  * Automatically synthesizes missing parent folders on the fly.
* **Unix Properties Preservation:** Transparently preserves and restores Symlinks, Hardlinks, Unix permissions, UID/GID (numeric & string), timestamps, and special files (Devices, FIFOs).

## Usage

### 1. Standard Drop-in Usage

Simply replace the import path.

```go
import "github.com/unxed/tar"

// Use exactly like the standard library
r, err := tar.OpenReader("archive.tar.gz")
if err != nil {
	log.Fatal(err)
}
defer r.Close()
```

### 2. High-Speed Random Access via fs.FS

Generate a SQLite index once and enjoy O(1) random access reads (uncompressed) and full `io/fs` compatibility.

```go
package main

import (
	"io/fs"
	"log"

	"github.com/unxed/tar"
)

func main() {
	// Automatically generates index.sqlite on first run if missing
	tfs, err := tar.NewFS("archive.tar", "index.sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer tfs.Close()

	// Perform random-access ReadFile
	data, err := fs.ReadFile(tfs, "folder/file.txt")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("File contents:", string(data))
}
```

### 3. Concurrent Extraction

```go
e, err := tar.NewExtractor("archive.tar.gz", "/path/to/dst", tar.WithExtractorConcurrency(8))
if err != nil {
	log.Fatal(err)
}
defer e.Close()

if err := e.Extract(context.Background()); err != nil {
	log.Fatal(err)
}
```

### 4. Append files to TAR

```go
f, err := os.OpenFile("archive.tar", os.O_RDWR, 0644)
if err != nil {
	log.Fatal(err)
}
defer f.Close()

updater, err := tar.NewUpdater(f, tar.APPEND_MODE_OVERWRITE)
if err != nil {
	log.Fatal(err)
}

err = updater.Append("newfile.txt", 4, []byte("data"))
if err != nil {
	log.Fatal(err)
}
updater.Close()
```

## License

This project is released under the **BSD-3-Clause License**. See the `LICENSE` file for details.

## Acknowledgements

Please see `CREDITS.md` for a detailed list of third-party acknowledgements and inspirations.
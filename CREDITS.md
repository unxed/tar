# Credits and Third-Party Licenses

This project (`github.com/unxed/tar`) is inspired by multiple outstanding open-source projects in the Go and Python ecosystems. We extend our deepest gratitude to the original authors.

Below is the list of projects whose concepts have been used in this library, along with their respective licenses.

---

### 1. Go Standard Library (`archive/tar`)
**Copyright:** (c) 2009 The Go Authors. All rights reserved.
**License:** BSD-3-Clause

The baseline tape archiving reading/writing specifications and basic header mappings are built atop the standard `archive/tar` library.

### 2. klauspost/compress
**Copyright:** (c) 2012 Klaus Post
**License:** BSD-3-Clause
**Source:** https://github.com/klauspost/compress

Provides high-speed `GZIP` and highly-parallelized `Zstandard` compression stream wrappers, serving as the basis for our high-throughput compressed streams.

### 3. saracen/fastzip
**Copyright:** (c) 2019 Arran Walker
**License:** MIT License
**Source:** https://github.com/saracen/fastzip

The concepts of parallel extractors and concurrent file writing (concurrency model using bounded goroutine pools) in `extractor.go` are directly adapted from `fastzip`.

### 4. mxmlnkn/ratarmount
**Copyright:** (c) 2019-2022 Maximilian Knespel
**License:** MIT License
**Source:** https://github.com/mxmlnkn/ratarmount

The database schema, indexing logic, on-the-fly missing parent directories synthesized folders generation, and `offsetheader` deduplication ideas are modeled directly after the `ratarmount` Python implementation.

### 5. Mark Adler's zran.c
**Copyright:** (C) 2005, 2012, 2018, 2023 Mark Adler
**License:** zlib License
**Source:** https://github.com/madler/zlib/blob/master/examples/zran.c

The binary `GZIDX` index format specification, the 32KB sliding window (dictionary) checkpointing concepts, and the bit-shifting seek-prime algorithm are based directly on the design of Mark Adler's `zran.c`.

### 6. forrestfwilliams/zran
**Copyright:** (c) 2023 Forrest Williams
**License:** zlib/libpng License
**Source:** https://github.com/forrestfwilliams/zran

The binary parsing logic of `GZIDX` index files and the structure of point-based checkpoints are based on the Python implementation of the `zran` wrapper.

### 7. unxed/zip
**Copyright:** (c) 2026 unxed
**License:** BSD-3-Clause
**Source:** https://github.com/unxed/zip

The overall library API structure, the `Updater` (append mode) design patterns, the testing methodologies, and project formatting were directly inspired by the `unxed/zip` advanced archive library.

---

This library is released under the 3-clause BSD license.
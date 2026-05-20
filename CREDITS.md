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

Provides high-speed `GZIP` and highly-parallelized `Zstandard` compression stream wrappers.

### 3. saracen/fastzip
**Copyright:** (c) 2019 Arran Walker
**License:** MIT License
**Source:** https://github.com/saracen/fastzip

The concepts of parallel extractors and concurrent file writing (concurrency model using goroutines) in `extractor.go` are directly adapted from `fastzip`.

### 4. mxmlnkn/ratarmount
**Copyright:** (c) 2019-2022 Maximilian Knespel
**License:** MIT License
**Source:** https://github.com/mxmlnkn/ratarmount

The database schema, indexing logic, on-the-fly missing parent directories synthesized folders generation, and `offsetheader` deduplication ideas are modeled directly after the `ratarmount` Python implementation.

---

This library is released under the 3-clause BSD license.
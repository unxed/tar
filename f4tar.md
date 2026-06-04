# f4 TAR Extensions Specification (Version 0.8)

## 1. Abstract
The **f4 TAR Extensions** provide a set of standardized PAX headers, methodologies, and an embedded metadata and indexing format (F4SS) designed to enhance cross-platform file system fidelity and performance within standard TAR archives. These extensions were originally developed for the `unxed/tar` golang library used in [f4](https://github.com/unxed/f4) — a cross-platform, asynchronous Far Manager clone. All extensions are backward-compatible with standard TAR utilities.

## 2. Cross-Platform Metadata (PAX Extensions)

### 2.1. Unix Extended Attributes (xattrs)
POSIX Extended Attributes, SELinux contexts, and POSIX ACLs are preserved using standard `SCHILY` and `LIBARCHIVE` PAX extended header records. This ensures compatibility with GNU `tar` and `bsdtar`.
- **Keys:** `SCHILY.xattr.<name>` or `LIBARCHIVE.xattr.<name>`
- **Value:** Raw binary/UTF-8 data.

### 2.2. Windows Security Descriptors (NTFS ACLs)
Standard TAR has no native mapping for complex NTFS Security Descriptors. The f4 extension encodes these in a custom PAX record to ensure exact NTFS permission preservation across platforms.
- **Key:** `MSWINDOWS.raw_sd`
- **Value:** Standard Base64-encoded string of the raw Windows Security Descriptor (self-relative format).

## 3. Incremental Sync Support (GNU Dumpdir)
f4 extensions adopt the GNU `tar` dumpdir format (`Typeflag: 'D'`, `TypeGNUDumpDir`) to support incremental restores and mirroring.
- **Payload:** A list of active files/directories, formatted as `[tag byte][filename]\0`, terminated by an extra `\0`.
- **Behavior:** During extraction in incremental mode, any file present in the target directory but missing from the corresponding GNU Dumpdir manifest MUST be deleted.

## 4. F4 Embedded TAR Structure (F4SS)

### 4.1. Architecture
A standard TAR archive consists of a series of file records, ending with an end-of-archive marker (two consecutive 512-byte blocks filled with binary zeros). Compression utilities (like GZIP, BZIP2, or ZSTD) support concatenated independent streams.

The F4SS profile organizes the archive into three consecutive streams:

```
+----------------------------------------------------------------+
| Stream 1: Original TAR Data                                    |
| (Files, directories, symlinks, ending with 2x512 zero blk)     |
+----------------------------------------------------------------+
| Stream 2: Metadata & Payload Stream (TAR)                      |
| (Contains .tarext/ directory with indexes and custom payloads) |
+----------------------------------------------------------------+
| Stream 3: Magic Footer                                         |
| (Fixed-size block pointing to Stream 2 offset and size)        |
+----------------------------------------------------------------+
```

#### 4.1.1. Stream 1 (Original TAR Data)
The first stream is a standard, fully valid compressed or uncompressed TAR archive.
- It contains all the archived directories, files, links, and metadata.
- **For GZIP archives utilizing chunked compression, this stream MAY include the standard `dictzip` chunk index natively within the GZIP `FEXTRA` header (Subfield ID 'RA') to guarantee out-of-the-box compatibility with native dictzip utilities.**
- It ends with two 512-byte zero blocks.
- **Behavior:** Standard utilities decode this stream, hit the zero blocks (EOF), and stop processing.

#### 4.1.2. Stream 2 (The Metadata and Payload Stream)
The second stream is an independent, concatenated compressed (or uncompressed) TAR archive. It serves as an extensible metadata container, conceptually similar to ZIP extra fields, but without the 64 KB size limitation.

To avoid namespace collisions, all metadata files and payloads inside Stream 2 MUST be placed within a reserved root-level directory named `.tarext/`.

##### Standard Payloads:
F4SS structures metadata inside Stream 2 under a unified taxonomy representing industry-standard specifications:

*   **`.tarext/ratarmount/index.sqlite`** or **`.tarext/ratarmount/index.arcidx`**
    *   **Description:** The filesystem metadata index mapping logical file paths to their uncompressed offsets. `index.sqlite` uses the classic SQLite schema, while `index.arcidx` uses the lightweight FlatBuffers schema (as standardized in [ratarmount#192](https://github.com/mxmlnkn/ratarmount/issues/192)).
*   **`.tarext/GZIDX/index.gzidx`**
    *   **Description:** A raw binary `GZIDX` index for stateful random access inside continuous GZIP streams (spec defined in Section 4.3.1).
*   **`.tarext/dictzip/index.dzidx`**
    *   **Description:** A raw binary chunk size index for stateless random access inside chunked (flushed) streams for formats other than GZIP (such as ZSTD) where native headers cannot be used (spec defined in Section 4.3.2).

##### Extensibility and Custom Payloads:
Third-party developers and archivers can define their own payloads by placing files inside `.tarext/`:
*   **Path Scheme:** `.tarext/<vendor_or_project>/<payload_name>`

Because Stream 2 is itself a standard TAR archive, individual payloads can range from small text configurations to gigabytes of auxiliary binary data, limited only by the underlying TAR specification.

#### 4.1.3. Stream 3 (The Magic Footer)
A small, fixed-size trailing block placed at the very end of the file. It provides the exact physical offset and size of Stream 2, enabling $O(1)$ lookup of the metadata stream.

### 4.2. Magic Footer Specifications

To prevent trailing-garbage warnings during standard decompression, the Magic Footer must conform to the underlying compression container's requirements.

#### 4.2.1. Uncompressed Store (Physical Footer)
For uncompressed `.tar` files, a 24-byte block is appended to the very end of the file:
```
[8 bytes: uint64 Little Endian - Compressed offset of Stream 2]
[8 bytes: uint64 Little Endian - Compressed size of Stream 2]
[8 bytes: ASCII characters - "F4IDX\x00\x00\x00" signature]
```

#### 4.2.2. Zstandard (ZSTD) Skippable Frame
For `.tar.zst` files, a standard ZSTD skippable frame is appended to the end. Since the frame magic is `0x184D2A50`, the decoder safely skips it.
- Size: 32 bytes.
- Layout:
  - `[0:4]` - `0x184D2A50` (uint32 Little Endian, Skippable Frame Magic)
  - `[4:8]` - `24` (uint32 Little Endian, Frame Payload size)
  - `[8:16]` - `Compressed offset of Stream 2` (uint64 Little Endian)
  - `[16:24]` - `Compressed size of Stream 2` (uint64 Little Endian)
  - `[24:32]` - `"F4IDX\x00\x00\x00"` (ASCII Signature)

#### 4.2.3. GZIP Extra Field empty stream
For `.tar.gz` files, a standard empty GZIP stream containing a custom `FEXTRA` subfield is appended.
- Size: 53 bytes.
- Layout:
  - `[0:2]` - `0x1f8b` (GZIP Magic)
  - `[2]` - `0x08` (DEFLATE compression method)
  - `[3]` - `0x04` (FEXTRA flag set)
  - `[4:10]` - MTIME, XFL, OS (set to 0)
  - `[10:12]` - `28` (uint16 Little Endian, XLEN)
  - `[12:14]` - `'F', '4'` (ASCII Subfield ID)
  - `[14:16]` - `24` (uint16 Little Endian, Subfield Length)
  - `[16:24]` - `Compressed offset of Stream 2` (uint64 Little Endian)
  - `[24:32]` - `Compressed size of Stream 2` (uint64 Little Endian)
  - `[32:40]` - `"F4IDX\x00\x00\x00"` (ASCII Signature)
  - `[40:45]` - `0x01, 0x00, 0x00, 0xff, 0xff` (Valid empty DEFLATE block with BFINAL=1, BTYPE=00)
  - `[45:53]` - `CRC32=0, ISIZE=0` (uint32 Little Endian fields of the GZIP footer)

### 4.3. Raw Compression Seek Indexes

To decouple filesystem metadata from the underlying compression layer, F4SS standardizes raw binary index formats stored within dedicated directories in Stream 2 or natively in Stream 1 headers.

#### 4.3.1. Continuous GZIP Index (`.tarext/GZIDX/index.gzidx`)
This format is binary-compatible with Mark Adler's `zran.c` and is used to index unmodified, continuously compressed GZIP streams. It stores periodic checkpoints containing the 32KB sliding window (dictionary) required to resume decompression at any arbitrary point.

##### Header Layout (35 bytes, Little Endian):
- `[0:5]` - `"GZIDX"` (ASCII Signature)
- `[5]` - `uint8` version (set to 1)
- `[6]` - `uint8` flags (set to 0)
- `[7:15]` - `uint64` compressed stream size
- `[15:23]` - `uint64` uncompressed stream size
- `[23:27]` - `uint32` spacing interval (typically 1MB to 4MB)
- `[27:31]` - `uint32` window size (typically 32768)
- `[31:35]` - `uint32` number of checkpoints ($N$)

##### Checkpoints List ($N \times 18$ bytes, Little Endian):
Repeated $N$ times immediately following the header:
- `[0:8]` - `uint64` compressed physical offset
- `[8:16]` - `uint64` uncompressed logical offset
- `[16]` - `uint8` bit offset (0-7, specifies the starting bit inside the byte)
- `[17]` - `uint8` hasData flag (1 if a dictionary window is present, 0 if not)

##### Dictionary Windows ($M \times 32768$ bytes):
Immediately following the Checkpoints list, there are $M$ raw 32KB buffers, where $M$ is the number of checkpoints where `hasData == 1`.

---

#### 4.3.2. Chunked Dictzip Index for Custom Formats (`.tarext/dictzip/index.dzidx`)
For formats other than GZIP (such as ZSTD) where native dictzip headers are impossible to implement, a compatible chunked seek index is stored in Stream 2. Since each chunk is independent, no sliding dictionary windows are required.

##### Header Layout (32 bytes, Little Endian):
- `[0:5]` - `"DZIDX"` (5 bytes ASCII Signature)
- `[5]` - `uint8` version (set to 1)
- `[6:8]` - `uint16` entry size in bytes (typically `2` for 16-bit chunk sizes or `4` for 32-bit chunk sizes)
- `[8:12]` - `uint32` chunk size (uncompressed interval, e.g., 65536)
- `[12:16]` - `uint32` chunk count ($P$)
- `[16:24]` - `uint64` total uncompressed stream size
- `[24:32]` - `uint64` total compressed stream size

##### Chunk Sizes ($P \times \text{entry\_size}$ bytes, Little Endian):
An array of $P$ integers (each of size `entry_size` bytes), representing the compressed size of each chunk in bytes. The layout and logic mirror the classic `dictzip` structure to maintain conceptual parity.

---

### 4.4. Indexing Strategies (Continuous vs. Chunked)

To support $O(1)$ random access within compressed streams, F4SS accommodates two primary strategies depending on the file container and the user's performance needs:

1.  **Continuous / Stateful Indexing (e.g., GZIDX):** The archive is compressed normally to maximize the compression ratio. The index stores periodic checkpoints, which for algorithms like DEFLATE include the 32KB sliding window (dictionary) required to resume decompression at that point.
2.  **Chunked / Flushed Indexing (dictzip style):** The compressor periodically flushes its state (e.g., using `Z_FULL_FLUSH` in zlib, or using independent frame boundaries in ZSTD) at fixed uncompressed intervals. While this slightly reduces the overall compression ratio, it allows the index to be extremely lightweight and makes seeking significantly faster.
    - **For GZIP streams:** The index MUST be stored natively in the GZIP header of Stream 1 using standard `dictzip` format.
    - **For other streams (e.g. ZSTD):** The index MUST be stored as a `.tarext/dictzip/index.dzidx` payload in Stream 2.

### 4.5. Implementation Guidelines
- **Modifications/Updates:** If a standard archiver appends files to the archive (using `tar -r`), it will find the first EOF zeros (end of Stream 1), overwrite them, and write new files. This inherently **destroys/overwrites** the index and metadata, preventing state desynchronization. F4-aware readers MUST check if the physical file size exceeds the expected offsets in the footer, treating the metadata stream as stale if they do.

### 4.6. Comparison to Prior Art

#### 4.6.1. dictzip Compatibility and Design
`dictzip` pioneered random access for GZIP by forcing chunked compression and storing block sizes in the GZIP `FEXTRA` header. 

F4SS adopts this exact methodology. For GZIP archives, F4SS avoids inventing custom extensions, instead prioritizing native compatibility with the existing `dictzip` ecosystem. If an archive is GZIP-compressed, the `dictzip` index is embedded natively within the GZIP headers of Stream 1. 

For other formats (such as `.tar.zst`), where native `dictzip` headers are not supported by the container, F4SS replicates the `dictzip` block-size architecture within the `.tarext/dictzip/index.dzidx` file inside Stream 2. This unifies the seek mechanics across different compression formats while ensuring native utilities remain fully supported.

#### 4.6.2. Pixz (TPXZ)
While `pixz` (parallel indexed XZ) pioneered the concept of embedding indexes for chunked compression after the TAR EOF marker, its architecture is tightly coupled to the block-based design of the XZ container format and can not be used for example with zStd.

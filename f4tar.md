# f4 TAR Extensions Specification (Version 0.6)

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
+-----------------------------------------------------------+
| Stream 1: Original TAR Data                               |
| (Files, directories, symlinks, ending with 2x512 zero blk)|
+-----------------------------------------------------------+
| Stream 2: Metadata & Payload Stream (TAR)                 |
| (Contains .f4/ directory with indexes and custom payloads)|
+-----------------------------------------------------------+
| Stream 3: Magic Footer                                    |
| (Fixed-size block pointing to Stream 2 offset and size)   |
+-----------------------------------------------------------+
```

#### 4.1.1. Stream 1 (Original TAR Data)
The first stream is a standard, fully valid compressed or uncompressed TAR archive.
- It contains all the archived directories, files, links, and metadata.
- It ends with two 512-byte zero blocks.
- **Behavior:** Standard utilities decode this stream, hit the zero blocks (EOF), and stop processing.

#### 4.1.2. Stream 2 (The Metadata and Payload Stream)
The second stream is an independent, concatenated compressed (or uncompressed) TAR archive. It serves as an extensible metadata container, conceptually similar to ZIP extra fields, but without the 64 KB size limitation.

To avoid namespace collisions, all metadata files and payloads inside Stream 2 MUST be placed within a reserved root-level directory named `.f4/`.

##### Standard Payloads:
*   **.f4/index.db**
    *   **Description:** A `ratarmount`-compatible SQLite database containing the pre-calculated offsets and metadata of all entries in Stream 1. (To be replaced or augmented by a more compact format in the future, see https://github.com/mxmlnkn/ratarmount/issues/192).

##### Extensibility and Custom Payloads:
Third-party developers and archivers can define their own payloads by placing files inside the `.f4/ext/` namespace:
*   **Path Scheme:** `.f4/ext/<vendor_or_project>/<payload_name>`

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
- **Total Size:** 32 bytes.
- **Layout:**
  - `[0:4]` - `0x184D2A50` (uint32 Little Endian, Skippable Frame Magic)
  - `[4:8]` - `24` (uint32 Little Endian, Frame Payload size)
  - `[8:16]` - `Compressed offset of Stream 2` (uint64 Little Endian)
  - `[16:24]` - `Compressed size of Stream 2` (uint64 Little Endian)
  - `[24:32]` - `"F4IDX\x00\x00\x00"` (ASCII Signature)

#### 4.2.3. GZIP Extra Field empty stream
For `.tar.gz` files, a standard empty GZIP stream containing a custom `FEXTRA` subfield is appended.
- **Total Size:** 53 bytes.
- **Layout:**
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

### 4.3. Implementation Guidelines
- **Modifications/Updates:** If a standard archiver appends files to the archive (using `tar -r`), it will find the first EOF zeros (end of Stream 1), overwrite them, and write new files. This inherently **destroys/overwrites** the index and metadata, preventing state desynchronization. F4-aware readers MUST check if the physical file size exceeds the expected offsets in the footer, treating the metadata stream as stale if they do.

### 4.4. Comparison to Pixz (TPXZ)

While `pixz` (parallel indexed XZ) pioneered the concept of embedding indexes after the TAR EOF marker, its architecture is tightly coupled to the block-based design of the XZ container format.

#### 4.4.1. The Block-Level Decoding Limitation
`pixz` compresses its index as a custom LZMA2 block and appends it before the XZ Index. Reading this requires utilizing the low-level `lzma_block_decoder` API of `liblzma`.
In pure-Go environments (like the standard `ulikunitz/xz` library), block-level decoding APIs are typically not exposed. A pure Go reader would either have to fall back to CGO or decompress the entire archive sequentially to read the trailing block, negating the $O(1)$ random-access property.

#### 4.4.2. Why F4SS is Designed for Pure-Go Ecosystems
By utilizing **concatenated compression streams**, F4SS bypasses container-specific block limitations:
1. **Container Agnostic:** F4SS works with GZIP, ZSTD, BZIP2, and uncompressed TAR files.
2. **Pure Go Friendly:** High-level Go decompressors (such as `gzip.NewReader` or `zstd.NewReader`) can natively decode the concatenated metadata stream (Stream 2) as an independent entity when pointed to by a simple `io.SectionReader`. No custom block parsers or CGO dependencies are required.
3. **$O(1)$ Lookup Efficiency:** Instead of parsing a potentially massive global index to find the last block, F4SS reads a small, fixed-size footer at the absolute physical end of the file in exactly one seek operation.
4. **Standardized Extensibility:** By leveraging a nested TAR archive inside Stream 2, any standard TAR library can be used to read, write, or list additional metadata payloads without requiring custom TLV parsers or proprietary container structures.

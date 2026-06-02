# F4 Embedded TAR Index Format Specification (Version 0.2)

## 1. Abstract
The **F4 Embedded TAR Index Format** (also known as **F4 Shadow Stream** or **F4SS**) provides a standard for embedding a search index directly inside compressed or uncompressed TAR archives. This format is fully backward-compatible: standard utilities (such as GNU `tar`) ignore the index and extract the archive normally, while F4-aware applications can instantly retrieve the index in $O(1)$ time.

---

## 2. Technical Architecture

A standard TAR archive consists of series of file records, ending with an end-of-archive marker (two consecutive 512-byte blocks filled with binary zeros). Compression utilities (like GZIP, BZIP2, or ZSTD) support concatenated independent streams.

The F4SS profile organizes the archive into three consecutive streams:

### 2.1. Stream 1 (Original TAR Data)
The first stream is a standard, fully valid compressed or uncompressed TAR archive.
- It contains all the archived directories, files, links, and metadata.
- It ends with two 512-byte zero blocks.
- **Behavior:** Standard utilities decode this stream, hit the zero blocks (EOF), and stop processing.

### 2.2. Stream 2 (The Shadow Index)
The second stream is an independent, concatenated compressed (or uncompressed) TAR archive containing exactly one file: `.f4.arcidx`.
- Filename: `.f4.arcidx` (uncompressed, regular file).
- Payload: A `ratarmount`-compatible SQLite database containing the pre-calculated offsets and metadata of all entries in Stream 1.
- It also ends with two 512-byte zero blocks.

### 2.3. Stream 3 (The Magic Footer)
A small, fixed-size trailing block placed at the very end of the file. It provides the exact physical offset and size of Stream 2, enabling $O(1)$ lookup.

---

## 3. Magic Footer Specifications

To prevent trailing-garbage warnings during standard decompression, the Magic Footer must conform to the underlying compression container's requirements.

### 3.1. Uncompressed Store (Physical Footer)
For uncompressed `.tar` files, a 24-byte block is appended to the very end of the file:
```
[8 bytes: uint64 Little Endian - Compressed offset of Stream 2]
[8 bytes: uint64 Little Endian - Compressed size of Stream 2]
[8 bytes: ASCII characters - "F4IDX\x00\x00\x00" signature]
```

### 3.2. Zstandard (ZSTD) Skippable Frame
For `.tar.zst` files, a standard ZSTD skippable frame is appended to the end. Since the frame magic is `0x184D2A50`, the decoder safely skips it.
- **Total Size:** 32 bytes.
- **Layout:**
  - `[0:4]` - `0x184D2A50` (uint32 Little Endian, Skippable Frame Magic)
  - `[4:8]` - `24` (uint32 Little Endian, Frame Payload size)
  - `[8:16]` - `Compressed offset of Stream 2` (uint64 Little Endian)
  - `[16:24]` - `Compressed size of Stream 2` (uint64 Little Endian)
  - `[24:32]` - `"F4IDX\x00\x00\x00"` (ASCII Signature)

### 3.3. GZIP Extra Field empty stream
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

---

## 4. Implementation Guidelines
- **Modifications/Updates:** If a standard archiver appends files to the archive (using `tar -r`), it will find the first EOF zeros (end of Stream 1), overwrite them, and write new files. This inherently **destroys/overwrites** the index, preventing state desynchronization. F4-aware readers MUST check if the physical file size exceeds the expected offsets in the footer, treating the index as stale if they do.

---

## 5. Comparison to Pixz (TPXZ)

While `pixz` (parallel indexed XZ) pioneered the concept of embedding indexes after the TAR EOF marker, its architecture is tightly coupled to the block-based design of the XZ container format.

### 5.1. The Block-Level Decoding Limitation
`pixz` compresses its index as a custom LZMA2 block and appends it before the XZ Index. Reading this requires utilizing the low-level `lzma_block_decoder` API of `liblzma`.
In pure-Go environments (like the standard `ulikunitz/xz` library), block-level decoding APIs are typically not exposed. A pure Go reader would either have to fall back to CGO or decompress the entire archive sequentially to read the trailing block, negating the $O(1)$ random-access property.

### 5.2. Why F4SS is Superior for Pure-Go Ecosystems
By utilizing **concatenated compression streams**, F4SS bypasses container-specific block limitations:
1. **Container Agnostic:** F4SS works seamlessly with GZIP, ZSTD, BZIP2, and uncompressed TAR files.
2. **Pure Go Friendly:** High-level Go decompressors (such as `gzip.NewReader` or `zstd.NewReader`) can natively decode the concatenated index stream (Stream 2) as an independent entity when pointed to by a simple `io.SectionReader`. No custom block parsers or CGO dependencies are required.
3. **$O(1)$ Lookup Efficiency:** Instead of parsing a potentially massive global index to find the last block, F4SS reads a small, fixed-size footer at the absolute physical end of the file in exactly one seek operation.
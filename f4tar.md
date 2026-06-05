# f4 TAR Extensions Specification (Version 0.9)

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
A standard TAR archive consists of a series of file records, ending with an end-of-archive marker (at least two consecutive 512-byte blocks filled with binary zeros). Compression utilities (like GZIP, BZIP2, or ZSTD) support concatenated independent streams.

The F4SS profile organizes the archive into three consecutive streams:

```text
+----------------------------------------------------------------+
| Stream 1: Original TAR Data                                    |
| (Files, directories, symlinks, ending with >=2 zero blocks)    |
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
- It ends with at least two 512-byte zero blocks.
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
```text
[0:8]   - uint64 Little Endian - Compressed offset of Stream 2
[8:16]  - uint64 Little Endian - Compressed size of Stream 2
[16:24] - ASCII characters - "F4IDX\x00\x00\x00" signature
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

### 4.4. Indexing Strategies (Continuous vs. Chunked)

To support $O(1)$ random access within compressed streams, F4SS accommodates two primary strategies depending on the file container and the user's performance needs:

1.  **Continuous / Stateful Indexing (e.g., GZIDX):** The archive is compressed normally to maximize the compression ratio. The index stores periodic checkpoints, which for algorithms like DEFLATE include the 32KB sliding window (dictionary) required to resume decompression at that point.
2.  **Chunked / Flushed Indexing (dictzip style):** The compressor periodically flushes its state (e.g., using `Z_FULL_FLUSH` in zlib, or using independent frame boundaries in ZSTD) at fixed uncompressed intervals. While this slightly reduces the overall compression ratio, it allows the index to be extremely lightweight and makes seeking significantly faster.
    - **For GZIP streams:** The index MUST be stored natively in the GZIP header of Stream 1 using standard `dictzip` format.
    - **For other streams (e.g. ZSTD):** The index MUST be stored as a `.tarext/dictzip/index.dzidx` payload in Stream 2.

### 4.5. Implementation Guidelines
- **Modifications/Updates via standard tools:** If a standard archiver appends files to the archive (e.g., using `tar -r`), it will seek to the first EOF zero blocks (end of Stream 1) and overwrite them with new file records. This inherently **destroys and overwrites** Stream 2 (metadata) and Stream 3 (Magic Footer). The archive safely reverts to a standard, non-indexed TAR file. F4-aware readers MUST validate the Magic Footer signature at the absolute end of the file; if it is missing or the physical file size does not match the footer offsets, the F4SS metadata MUST be considered destroyed.

### 4.6. Comparison to Prior Art

#### 4.6.1. dictzip Compatibility and Design
`dictzip` pioneered random access for GZIP by forcing chunked compression and storing block sizes in the GZIP `FEXTRA` header. 

F4SS adopts this exact methodology. For GZIP archives, F4SS avoids inventing custom extensions, instead prioritizing native compatibility with the existing `dictzip` ecosystem. If an archive is GZIP-compressed, the `dictzip` index is embedded natively within the GZIP headers of Stream 1. 

For other formats (such as `.tar.zst`), where native `dictzip` headers are not supported by the container, F4SS replicates the `dictzip` block-size architecture within the `.tarext/dictzip/index.dzidx` file inside Stream 2. This unifies the seek mechanics across different compression formats while ensuring native utilities remain fully supported.

#### 4.6.2. Pixz (TPXZ)
While `pixz` (parallel indexed XZ) pioneered the concept of embedding indexes for chunked compression after the TAR EOF marker, its architecture is tightly coupled to the block-based design of the XZ container format and can not be used for example with zStd.

## 5. F4Crypt: Encrypted Archives (AES-256)

### 5.1. Abstract and Backward Compatibility
To provide strong encryption while maintaining absolute backward compatibility with legacy TAR utilities (which lack native encryption flags), F4SS introduces the **F4Crypt** encapsulation method.

When an archive is encrypted, the original files are hidden from legacy tools. If a user extracts an F4Crypt archive using standard `tar`, `7z`, or `WinRAR`, they will not see encrypted garbage or encounter extraction errors. Instead, the archive will cleanly extract a single plaintext stub file warning them that the archive is encrypted. 

F4-aware utilities will read the F4SS Magic Footer, locate the encrypted payload in Stream 2, prompt the user for a password, and seamlessly mount or extract the encapsulated files.

### 5.2. Encrypted Archive Architecture
An encrypted F4SS archive is typically stored as an **uncompressed** outer `.tar` file, structured as follows:

1. **Stream 1 (The Stub):** A valid, uncompressed TAR stream containing exactly one file.
   - **Filename:** `README_ENCRYPTED.txt` (or a similar culturally appropriate filename).
   - **Content:** A human-readable message (e.g., *"This is an encrypted archive. Please use f4 or an F4SS-compatible tool to extract it."*).
   - Stream 1 MUST terminate with standard TAR EOF zero blocks.
2. **Stream 2 (The Payload):** An uncompressed TAR stream containing the cryptographic headers and the AES-encrypted inner archive.
   - All files MUST reside under `.tarext/f4crypt/`.
3. **Stream 3 (Magic Footer):** The standard F4SS Uncompressed Magic Footer (Section 4.2.1) pointing to Stream 2.

### 5.3. F4Crypt Payload Structure (Inside Stream 2)
Stream 2 contains the following files:

*   **`.tarext/f4crypt/crypto.hdr`**
    *   **Description:** A raw binary file containing key derivation parameters (Salt, Iterations), the AES Nonce (IV), and the MAC tag for integrity verification.
*   **`.tarext/f4crypt/payload.enc`**
    *   **Description:** The AES-256 encrypted byte stream of the *actual* (inner) archive. This inner archive MAY be compressed (e.g., a `.tar.gz` stream) prior to encryption.
*   **`.tarext/f4crypt/index.*.enc` (Optional)**
    *   **Description:** F4SS indexes (like `index.sqlite` or `index.gzidx`) MUST be encrypted using the same key and IV stream to prevent metadata leakage (such as file names, sizes, and offsets).

### 5.4. Cryptographic Primitives
F4Crypt prioritizes random access (seeking) without sacrificing security, heavily inspired by ZIP's AE-x specification but optimized for large TAR streams.

- **Cipher:** `AES-256` in **CTR (Counter) Mode**. CTR mode transforms a block cipher into a stream cipher. This allows $O(1)$ random access decryption at any byte offset, perfectly synergizing with F4SS seek indexes (like SQLite and GZIDX).
- **Key Derivation (KDF):** `PBKDF2-HMAC-SHA256`. Generates a 32-byte (256-bit) master key from the user's password.
- **Authentication:** `HMAC-SHA256`. Calculated over the entire ciphertext of `payload.enc`. Archivers SHOULD verify this MAC upon full extraction. When mounting the archive via FUSE/VFS, MAC verification MAY be deferred or skipped to allow instant mounting.

### 5.5. `crypto.hdr` Binary Layout
The `crypto.hdr` file is exactly **93 bytes** long (Little Endian):

- `[0:6]`   - `"F4CRPT"` (6 bytes ASCII Signature)
- `[6]`     - `uint8` Version (`0x01`)
- `[7]`     - `uint8` KDF Algorithm (`0x01` = PBKDF2-HMAC-SHA256)
- `[8]`     - `uint8` Cipher (`0x01` = AES-256-CTR)
- `[9:13]`  - `uint32` KDF Iterations (Minimum RECOMMENDED: `600000`)
- `[13:45]` - `32 bytes` Cryptographically secure random Salt for KDF
- `[45:61]` - `16 bytes` AES-CTR Nonce / Initialization Vector (IV)
- `[61:93]` - `32 bytes` HMAC-SHA256 Authentication Tag (Calculated over the ciphertext of `payload.enc`)

### 5.6. Decryption and Random Access Workflow
Because AES-CTR operates as a stream cipher, decrypting any specific block inside `payload.enc` is highly efficient:
1. Parse `crypto.hdr` and derive the 32-byte AES key using the user-provided Password, Salt, and Iteration count.
2. Read the 16-byte base IV.
3. If the user requests data at an unencrypted byte offset $X$ within `payload.enc`:
   - Calculate the AES block counter offset: $Block = \lfloor X / 16 \rfloor$.
   - Calculate the byte offset within the block: $Rem = X \pmod{16}$.
   - Increment the base IV by $Block$ (treated as a 128-bit big-endian integer) to get the current counter block.
   - Encrypt the counter block with AES-256 to generate the 16-byte keystream.
   - XOR the ciphertext from `payload.enc` starting at offset $X$ with the keystream starting at index $Rem$.

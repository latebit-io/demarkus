# The Mark Protocol Specification

**Version**: 1.0 Draft  
**Date**: 2026-02-17  
**Status**: Working Draft  
**Project**: Demarkus

## Abstract

The Mark Protocol is an application-layer protocol for the transfer of markdown documents over QUIC. It provides a document-centric, privacy-first alternative to HTTP, designed around immutable versioning, human-readable wire formats, and mandatory encryption. The protocol uses text-based verbs and status values, YAML frontmatter for metadata, and a SHA-256 hash chain for version integrity verification.

## Status of This Document

This is a working draft specification for the Mark Protocol version 1.0. It documents the normative behaviour of the protocol as currently defined. Features described in the project design document (DESIGN.md) that are not yet specified here — including APPEND, ARCHIVE, SEARCH, authentication, and federation — are considered future extensions and are not part of this specification.

## 1. Terminology

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

Additional terms:

- **Document**: A markdown file served by a Mark Protocol server.
- **Frontmatter**: A YAML metadata block delimited by `---` lines.
- **Version**: An immutable snapshot of a document's content at a point in time.
- **Current version**: The most recent version of a document.
- **Hash chain**: A sequence of cryptographic hashes linking each version to its predecessor.

## 2. Protocol Overview

### 2.1. Scheme

The Mark Protocol uses the URI scheme `mark`. A conforming URI has the form:

```
mark://host[:port]/path
```

If the port is omitted, the default port 6309 is assumed.

### 2.2. Transport

The Mark Protocol runs exclusively over QUIC (RFC 9000). There is no plaintext fallback. TLS 1.3 is mandatory.

### 2.3. Content Format

All document content is markdown (CommonMark). Metadata is encoded as YAML frontmatter. There is no support for executable content — no scripts, no embedded code execution, no client-side dynamic behaviour.

## 3. Transport Layer

### 3.1. QUIC

A Mark Protocol server MUST accept QUIC connections. A Mark Protocol client MUST connect using QUIC.

Each request-response exchange takes place on a single bidirectional QUIC stream. The client opens a stream, writes the complete request, signals the end of writing, and reads the complete response until the stream is closed by the server.

### 3.2. Port

The default port for Mark Protocol servers is **6309** (UDP). Servers MAY listen on alternative ports. Clients MUST support specifying a non-default port in the URI.

### 3.3. TLS

All Mark Protocol connections MUST use TLS 1.3 or later. Servers MUST NOT accept unencrypted connections. Clients MUST NOT send requests over unencrypted connections.

Servers SHOULD use certificates issued by a trusted certificate authority for production deployments. Servers MAY use self-signed certificates for development purposes.

### 3.4. ALPN

The Application-Layer Protocol Negotiation (ALPN) identifier for the Mark Protocol is:

```
mark
```

Servers MUST include `"mark"` in their TLS ALPN extension. Clients MUST request `"mark"` in the ALPN negotiation.

## 4. Request Format

### 4.1. Structure

A request consists of a request line, optional frontmatter, and an optional body:

```
VERB /path\n
[---\n
key: value\n
...\n
---\n]
[body]
```

The request line is a single text line terminated by a newline character (`\n`, not `\r\n`). It consists of a verb and a path separated by a single space character.

### 4.2. Request Line

The request line MUST have the form:

```
VERB /path\n
```

- The verb MUST be a known protocol verb (see Section 6).
- The path MUST begin with `/`.
- The path MUST NOT contain null bytes (`\0`), control characters (codepoints below 32 except horizontal tab `\t`), or the DEL character (codepoint 127).
- The maximum length of the request line is **4096 bytes**.

### 4.3. Request Metadata (Frontmatter)

If the line immediately following the request line is exactly `---`, the request includes metadata. Metadata lines follow until a closing `---` line is encountered. The content between the delimiters is parsed as YAML into a flat key-value map where all values are strings.

```
---\n
key1: value1\n
key2: value2\n
---\n
```

- Metadata is OPTIONAL for all verbs.
- The maximum size of the metadata block (excluding delimiters) is **65536 bytes** (64 KB).
- Servers MUST reject requests whose metadata exceeds this limit.

### 4.4. Request Body

Everything after the metadata closing delimiter (or after the request line if no metadata is present) constitutes the request body.

- The body is OPTIONAL for all verbs.
- For WRITE requests, the body contains the document content.
- There is no protocol-level size limit on the body; servers SHOULD enforce a maximum document size (see Section 12.3).

## 5. Response Format

### 5.1. Structure

A response consists of YAML frontmatter followed by an optional markdown body:

```
---\n
status: <status-value>\n
key: value\n
...\n
---\n
[body]
```

### 5.2. Response Frontmatter

All responses MUST include frontmatter. The frontmatter MUST include a `status` field.

All frontmatter values are strings. Implementations MUST parse frontmatter as `map[string]string` to prevent YAML type coercion of timestamps, numbers, and booleans.

### 5.3. Response Body

The body is everything following the closing frontmatter delimiter. It is markdown-formatted text.

- The body MAY be empty (e.g., for `not-modified` responses).
- Error responses SHOULD include a human-readable markdown body describing the error.

### 5.4. Error Body Format

Error responses SHOULD use the following body format:

```markdown
# <Status Title>

<Human-readable error message>
```

Where the status title is the status value with the first letter capitalised and hyphens replaced with spaces (e.g., `not-found` becomes `Not found`).

## 6. Verbs

### 6.1. FETCH

Retrieves a document.

**Request**:
```
FETCH /path\n
```

**Conditional request metadata** (OPTIONAL):
- `if-none-match`: An ETag value from a previous response.
- `if-modified-since`: An RFC 3339 timestamp from a previous response.

**Success response** (`ok`):
```
---
status: ok
modified: <RFC 3339 timestamp>
etag: <64-char hex SHA-256>
version: <integer>
---
<markdown body>
```

**Conditional response** (`not-modified`):

If the request includes `if-none-match` and it matches the current ETag, or if the request includes `if-modified-since` and the document has not been modified after that time, the server MUST respond with:

```
---
status: not-modified
---
```

The `not-modified` response MUST NOT include a body.

When both `if-none-match` and `if-modified-since` are present, the server MUST check `if-none-match` first. If it matches, `not-modified` is returned without checking `if-modified-since`.

**Version access**:

A path of the form `/doc.md/vN` (where N is a positive integer) requests a specific version. The response includes additional metadata:

```
---
status: ok
modified: <RFC 3339 timestamp>
version: <requested version>
current-version: <highest version number>
---
<markdown body>
```

**Errors**:
- `not-found`: The document does not exist.
- `server-error`: Internal error or the file exceeds the size limit.

**Special path**: `FETCH /health` is a health check endpoint. Servers MUST respond with `status: ok` and a body indicating server health.

### 6.2. LIST

Lists the contents of a directory.

**Request**:
```
LIST /path/\n
```

**Success response** (`ok`):
```
---
status: ok
entries: <count>
---
<markdown body with directory listing>
```

The body MUST be a markdown document containing a list of entries:
- Directories are listed as `- [name/](url-encoded-name/)`
- Files are listed as `- [name](url-encoded-name)`

Servers MUST exclude hidden files (names beginning with `.`) from directory listings.

Servers MUST impose a maximum entry count. The RECOMMENDED limit is **1000** entries. If the listing is truncated, the body SHOULD end with a note indicating truncation.

**Errors**:
- `not-found`: The directory does not exist, or the path refers to a file.
- `server-error`: Internal error.

### 6.3. VERSIONS

Retrieves the version history of a document.

**Request**:
```
VERSIONS /path\n
```

**Success response** (`ok`):
```
---
status: ok
total: <version count>
current: <highest version number>
chain-valid: <true|false>
chain-error: <error description>
---
<markdown body with version list>
```

The body MUST list all versions from newest to oldest:
```markdown
# Version History: /path

- [v3](/path/v3) - 2026-02-17T10:00:00Z
- [v2](/path/v2) - 2026-02-16T09:00:00Z
- [v1](/path/v1) - 2026-02-15T08:00:00Z
```

**Metadata**:
- `total`: The total number of versions.
- `current`: The highest version number.
- `chain-valid`: `"true"` if the hash chain is intact; `"false"` if any link is broken.
- `chain-error`: Present only when `chain-valid` is `"false"`. Contains a human-readable description of the first broken chain link.

Only documents with version history (written through the protocol) are served. Flat files without a `versions/` directory are treated as non-existent.

**Errors**:
- `not-found`: The document does not exist or has no version history.
- `server-error`: Internal error or versioning not configured.

### 6.4. WRITE

Creates a new immutable version of a document. Requires authentication.

**Request**:
```
WRITE /path\n
---\n
auth: <raw-token>\n
---\n
<document content>
```

The `auth` metadata field is REQUIRED. The server hashes the raw token with SHA-256 and looks up the resulting hash in its token store. The token must grant the `write` operation on the requested path.

The request body is the document content. It is stored as-is (the server prepends its own store frontmatter; the original content is preserved verbatim).

**Success response** (`created`):
```
---
status: created
version: <new version number>
modified: <RFC 3339 timestamp>
---
```

The `created` response MUST NOT include a body.

**Behaviour**:
- Every WRITE creates a new version. The server MUST NOT modify or overwrite any existing version.
- If the document does not exist, version 1 is created.
- If the document exists, the version number is incremented from the current highest version.
- If the document exists as a flat file (no version history), the server MUST migrate the flat file to version 1 before creating version 2.

**Authentication errors**:
- `not-permitted`: No token store configured on the server (writes disabled).
- `unauthorized`: Missing `auth` field or token not recognised.
- `not-permitted`: Token does not grant `write` on the requested path.

**Other errors**:
- `not-found`: Path validation failed (e.g., path traversal attempt).
- `server-error`: Internal error, content exceeds size limit, or writing not configured.

## 7. Status Values

Status values are text strings. There are no numeric status codes.

| Value | Meaning |
|---|---|
| `ok` | Request succeeded. Body contains the requested content. |
| `created` | Write succeeded. A new version was created. |
| `not-modified` | Conditional request: the resource has not changed. No body. |
| `not-found` | The requested resource does not exist. |
| `unauthorized` | Missing or invalid authentication token. |
| `not-permitted` | Valid authentication but insufficient capability for the requested operation or path. |
| `server-error` | The server encountered an error processing the request. |

### 7.1. Future Status Values

The following status values are reserved for future use:

| Value | Intended meaning |
|---|---|
| `conflict` | Version conflict (e.g., simultaneous writes). |
| `bad-request` | Malformed request. |
| `too-large` | Document exceeds the size limit. |
| `unavailable` | Server temporarily cannot fulfil the request. |

## 8. Metadata Fields

### 8.1. Request Metadata

| Field | Applicable verbs | Format | Description |
|---|---|---|---|
| `if-none-match` | FETCH | 64-char hex string | ETag from a previous response. Enables conditional fetch. |
| `if-modified-since` | FETCH | RFC 3339 timestamp | Timestamp from a previous response. Enables conditional fetch. |
| `auth` | WRITE | String | Raw authentication token. The server hashes this with SHA-256 and looks up the hash in its token store. |

### 8.2. Response Metadata

| Field | Applicable verbs | Format | Description |
|---|---|---|---|
| `modified` | FETCH, WRITE | RFC 3339 timestamp | Document modification time (UTC, second precision). |
| `etag` | FETCH | 64-char lowercase hex | SHA-256 hash of the raw file bytes. |
| `version` | FETCH, WRITE | Decimal integer | Version number of the returned or created document. |
| `current-version` | FETCH (version access) | Decimal integer | Highest available version number. |
| `entries` | LIST | Decimal integer | Number of entries in the directory listing. |
| `total` | VERSIONS | Decimal integer | Total number of versions. |
| `current` | VERSIONS | Decimal integer | Highest version number. |
| `chain-valid` | VERSIONS | `true` or `false` | Whether the version hash chain is intact. |
| `chain-error` | VERSIONS | String | Description of chain verification failure. Present only when `chain-valid` is `false`. |

## 9. Versioning

### 9.1. Version Model

The Mark Protocol uses an append-only version model. Every write to a document creates a new version. Published versions are permanent and MUST NOT be modified or deleted. Version history is an append-only log.

Version numbers are positive integers starting at 1, monotonically increasing by 1 for each write.

### 9.2. Path-Based Version Access

Specific versions are accessed via the path structure:

```
/doc.md          → current version
/doc.md/v1       → version 1
/doc.md/v42      → version 42
```

The version segment MUST match the pattern `v` followed by a positive integer (no leading zeros required). `v0` is not a valid version. Paths with a version segment that does not match this pattern are treated as regular paths.

### 9.3. On-Disk Layout

Servers SHOULD store versioned documents using the following layout:

```
root/
  doc.md              ← symlink to versions/doc.md.v<current>
  versions/
    doc.md.v1
    doc.md.v2
    doc.md.v<N>
```

The current file (`doc.md`) SHOULD be a symbolic link to the latest version file. Version files reside in a `versions/` subdirectory at the same level as the document.

Version files are named `<filename>.v<N>` where N is the version number.

### 9.4. Version File Format

Each version file MUST be prefixed with a store-managed frontmatter block:

**Version 1** (genesis):
```
---
version: 1
---
<original document content>
```

**Version N (N > 1)**:
```
---
version: <N>
previous-hash: sha256-<64-char lowercase hex>
---
<original document content>
```

The store frontmatter is separate from any frontmatter that may exist in the original document content. The original content is stored verbatim after the store frontmatter closing delimiter.

### 9.5. Hash Chain

Each version file (except version 1) MUST include a `previous-hash` field in its store frontmatter. The value is the SHA-256 hash of the complete raw bytes of the previous version file (including that file's own store frontmatter), formatted as `sha256-` followed by 64 lowercase hexadecimal characters.

This forms a hash chain:

```
v1 (genesis)     v2                    v3
┌────────────┐   ┌─────────────────┐   ┌─────────────────┐
│ version: 1 │   │ version: 2      │   │ version: 3      │
│            │──►│ previous-hash:  │──►│ previous-hash:  │
│ content... │   │   sha256(v1)    │   │   sha256(v2)    │
└────────────┘   │ content...      │   │ content...      │
                 └─────────────────┘   └─────────────────┘
```

### 9.6. Chain Verification

To verify the integrity of a document's version history:

1. Read all version files, sorted by version number (oldest first).
2. For each version N > 1:
   a. Compute `sha256(<raw bytes of version N-1 file>)`.
   b. Format as `sha256-<hex>`.
   c. Compare with the `previous-hash` value in version N's store frontmatter.
   d. If they do not match, the chain is broken at version N.

If any version file has been modified after publication, the hash recorded in the next version will not match, and the tampering is detected.

### 9.7. Immutability Enforcement

Servers MUST NOT overwrite existing version files. Before writing a new version file, the server MUST verify that no file exists at the target path. If the target file already exists, the write MUST fail.

### 9.8. Flat File Migration

When a WRITE is performed on a document that exists as a flat file (no version history), the server MUST:

1. Create the versions directory if it does not exist.
2. Migrate the flat file content to `versions/<filename>.v1` with store frontmatter (`version: 1`, no `previous-hash`).
3. Create the new version as `versions/<filename>.v2` with a `previous-hash` referencing the hash of the migrated v1 file.
4. Update the current file to a symlink pointing to the new version.

## 10. Caching

### 10.1. ETag

The server MUST compute an ETag for every successful FETCH response. The ETag is the SHA-256 hash of the raw file bytes (before any frontmatter stripping), formatted as 64 lowercase hexadecimal characters.

### 10.2. Conditional Requests

Clients MAY include `if-none-match` and/or `if-modified-since` metadata in FETCH requests.

- `if-none-match`: If the value matches the current ETag, the server MUST respond with `not-modified`.
- `if-modified-since`: If the document has not been modified after the given RFC 3339 timestamp, the server MUST respond with `not-modified`.

When both are present, `if-none-match` takes precedence. If it matches, `not-modified` is returned without evaluating `if-modified-since`.

### 10.3. Not-Modified Response

A `not-modified` response MUST have an empty body and empty metadata (aside from the `status` field).

## 11. Security Considerations

### 11.1. Encryption

All Mark Protocol communication MUST be encrypted via TLS 1.3 or later. There is no plaintext mode. This ensures confidentiality and integrity of all document transfers.

### 11.2. Path Traversal

Servers MUST validate all request paths to prevent directory traversal attacks. The path validation algorithm MUST:

1. Normalise the path (resolve `.` and `..` segments).
2. Resolve symbolic links in the target path to detect symlink-based escapes.
3. Verify that the resolved absolute path is within the content root directory.

Servers MUST return `not-found` (not `not-permitted` or any other status) for path traversal attempts to avoid disclosing information about the filesystem structure outside the content root.

### 11.3. Size Limits

Servers MUST enforce the following limits:

| Resource | Limit |
|---|---|
| Request line | 4096 bytes |
| Request metadata | 65536 bytes (64 KB) |
| Document size (read and write) | 10 MB (RECOMMENDED) |
| Directory listing entries | 1000 (RECOMMENDED) |

### 11.4. No Tracking

The Mark Protocol is designed to minimise tracking. Conforming implementations:

- MUST NOT send user agent identification.
- MUST NOT send referrer information.
- MUST NOT use cookies or session identifiers.
- MUST NOT collect IP addresses beyond what QUIC requires for connection handling.
- SHOULD log only the operation, path, and status — no personally identifiable information.

### 11.5. No Client-Side Execution

The Mark Protocol serves markdown content only. There is no mechanism for executable content (scripts, active content, or client-side code execution). Clients MUST NOT execute any content received via the Mark Protocol.

### 11.6. Input Sanitisation

Servers MUST reject paths containing null bytes or control characters (codepoints below 32, except horizontal tab). Servers MUST sanitise all user-supplied strings before writing them to log files to prevent log injection attacks.

### 11.7. Symlink Safety

When the content directory or any document path involves symbolic links, the server MUST resolve all symlinks and verify that the final resolved path remains within the content root. Symlinks that escape the content root MUST be treated as `not-found`.

### 11.8. Authentication

The Mark Protocol uses capability-based token authentication. Tokens grant specific operations on specific path patterns — they do not identify users.

**Secure by default**: Servers MUST deny all write operations when no token store is configured. Reads do not require authentication.

**Token storage**: The server stores SHA-256 hashes of tokens, never the raw tokens themselves. The token store is a TOML file:

```toml
[tokens]
"sha256-a1b2c3d4..." = { paths = ["/docs/*"], operations = ["write"] }
"sha256-e5f6a7b8..." = { paths = ["/*"], operations = ["read", "write"], expires = "2026-12-31T23:59:59Z" }
```

**Token fields**:
- `paths`: Array of glob patterns. `*` matches any single path segment (not recursive).
- `operations`: Array of permitted operations (`read`, `write`).
- `expires`: OPTIONAL RFC 3339 timestamp. If present, the token is invalid after this time.

**Authentication flow**:
1. Client includes `auth: <raw-token>` in request metadata.
2. Server computes `sha256-<hex of SHA-256(raw-token)>`.
3. Server looks up the hash in its token store.
4. If not found: respond with `unauthorized`.
5. If found but the token does not grant the requested operation on the requested path: respond with `not-permitted`.
6. If authorised: proceed with the request.

**Token generation**: The `demarkus-token generate` tool creates cryptographically random tokens and appends their hashed entries to the token store file. The raw token is printed once and never stored by the server.

### 11.9. Versioned-Only Serving

Servers MUST only serve documents that have been written through the protocol (i.e., documents with a `versions/` directory containing at least one version file). Flat files placed directly on the filesystem without version history MUST be treated as `not-found`.

This ensures every served document has:
- An immutable version chain
- SHA-256 hash chain for tamper detection
- Proper store frontmatter

## 12. Future Extensions

The following features are planned but not part of this specification:

- **APPEND**: Add content to the end of a document.
- **ARCHIVE**: Remove a document from active serving while preserving version history.
- **SEARCH**: Full-text search across documents.
- **Content addressing**: Hash-based document retrieval independent of location.
- **Federation**: Cross-server content mirroring and discovery.
- **Subscriptions**: Notification of document changes.

These will be specified in future versions of this document.

## 13. Protocol Constants

| Constant | Value |
|---|---|
| Default port | 6309 (UDP) |
| ALPN identifier | `mark` |
| URI scheme | `mark` |
| TLS minimum version | 1.3 |
| Max request line | 4096 bytes |
| Max request metadata | 65536 bytes |
| Recommended max document size | 10 MB |
| Recommended max directory entries | 1000 |
| Hash algorithm | SHA-256 |
| Hash format | `sha256-<64 lowercase hex chars>` |

## 14. References

- RFC 2119 — Key words for use in RFCs to Indicate Requirement Levels
- RFC 9000 — QUIC: A UDP-Based Multiplexed and Secure Transport
- RFC 8446 — The Transport Layer Security (TLS) Protocol Version 1.3
- RFC 3339 — Date and Time on the Internet: Timestamps
- CommonMark Specification — https://spec.commonmark.org/
- YAML 1.2 Specification — https://yaml.org/spec/1.2/

---

*Mark Protocol Specification — "The web we want, not the web we got."*

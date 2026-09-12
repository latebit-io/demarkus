package answerbench

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/latebit-io/demarkus/protocol/store"
)

const corpusArchiveFormat = "demarkus-corpus-jsonl-gzip-v1"

type corpusEntry struct {
	Path     string
	Document store.StoredDocument
}

// CorpusManifest pins a private archive without including document content.
type CorpusManifest struct {
	Format            string `json:"format"`
	ID                string `json:"id"`
	Source            string `json:"source"`
	Artifact          string `json:"artifact"`
	SHA256            string `json:"sha256"`
	CorpusSHA256      string `json:"corpus_sha256"`
	CompressedBytes   int64  `json:"compressed_bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
	Documents         int    `json:"documents"`
	ActiveDocuments   int    `json:"active_documents"`
	Versions          int    `json:"versions"`
}

// PackOptions separates the ignored archive from its versionable manifest.
type PackOptions struct {
	Root, Archive, Manifest, ID, Source string
}

type byteCounter struct{ n int64 }

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func exportCorpus(ctx context.Context, root string, output io.Writer) (CorpusManifest, error) {
	var stats CorpusManifest
	hash, count := sha256.New(), &byteCounter{}
	enc := json.NewEncoder(io.MultiWriter(output, hash, count))
	err := store.New(root).ExportDocs(ctx, func(path string, doc store.StoredDocument) error {
		stats.Documents++
		stats.Versions += len(doc.Versions)
		if !doc.Archived {
			stats.ActiveDocuments++
		}
		return enc.Encode(corpusEntry{Path: path, Document: doc})
	})
	stats.CorpusSHA256 = fmt.Sprintf("sha256-%x", hash.Sum(nil))
	stats.UncompressedBytes = count.n
	return stats, err
}

// PackCorpus stores raw version bytes, archive state and modified times in gzip.
// Existing outputs are never overwritten; no live service or credential is needed.
func PackCorpus(ctx context.Context, opts *PackOptions) (manifest CorpusManifest, err error) {
	if opts.ID == "" || opts.Source == "" || opts.Root == "" || opts.Archive == "" || opts.Manifest == "" {
		return manifest, errors.New("pack requires root, archive, manifest, id and source")
	}
	file, err := os.OpenFile(opts.Archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return manifest, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(opts.Archive))
		}
	}()
	hash, count := sha256.New(), &byteCounter{}
	gz, zipErr := gzip.NewWriterLevel(io.MultiWriter(file, hash, count), gzip.BestCompression)
	if zipErr != nil {
		return manifest, errors.Join(zipErr, file.Close())
	}
	manifest, err = exportCorpus(ctx, opts.Root, gz)
	err = errors.Join(err, gz.Close(), file.Sync(), file.Close())
	if err != nil {
		return manifest, err
	}
	manifest.Format, manifest.ID, manifest.Source = corpusArchiveFormat, opts.ID, opts.Source
	manifest.Artifact = filepath.Base(opts.Archive)
	manifest.SHA256 = fmt.Sprintf("sha256-%x", hash.Sum(nil))
	manifest.CompressedBytes = count.n
	if err := validateCorpusManifest(&manifest); err != nil {
		return manifest, err
	}
	err = writeNewJSON(opts.Manifest, &manifest)
	return manifest, err
}

func writeNewJSON(path string, value any) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	return file.Sync()
}

func validateCorpusManifest(m *CorpusManifest) error {
	if m.Format != corpusArchiveFormat || m.Documents < 1 || m.ActiveDocuments < 1 || m.ActiveDocuments > m.Documents || m.Versions < m.Documents {
		return errors.New("invalid corpus manifest format or counts")
	}
	if m.CompressedBytes < 1 || m.CompressedBytes > 256<<20 || m.UncompressedBytes < 1 || m.UncompressedBytes > 1<<30 {
		return errors.New("archive exceeds supported bounds (256 MiB compressed, 1 GiB expanded)")
	}
	if len(m.SHA256) != 71 || len(m.CorpusSHA256) != 71 {
		return errors.New("manifest requires SHA-256 archive and corpus hashes")
	}
	return nil
}

func readBounded(path string, limit int64) (raw []byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	raw, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return raw, err
}

// RestoreCorpus verifies compressed bytes before importing into a new directory,
// then verifies the reconstructed store against the original corpus fingerprint.
func RestoreCorpus(ctx context.Context, manifestPath, archivePath, root string) (manifest CorpusManifest, err error) {
	raw, err := readBounded(manifestPath, 64<<10)
	if err != nil {
		return manifest, err
	}
	if err := decodeJSON(raw, &manifest); err != nil {
		return manifest, err
	}
	if err := validateCorpusManifest(&manifest); err != nil {
		return manifest, err
	}
	compressed, err := readBounded(archivePath, manifest.CompressedBytes)
	if err != nil {
		return manifest, err
	}
	if int64(len(compressed)) != manifest.CompressedBytes || digest(compressed) != manifest.SHA256 {
		return manifest, errors.New("corpus archive checksum/size mismatch")
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return manifest, fmt.Errorf("restore requires a new destination: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(root))
		}
	}()
	if err := importCorpus(ctx, compressed, root, &manifest); err != nil {
		return manifest, err
	}
	got, err := exportCorpus(ctx, root, io.Discard)
	if err != nil {
		return manifest, err
	}
	if got.CorpusSHA256 != manifest.CorpusSHA256 || got.Documents != manifest.Documents || got.ActiveDocuments != manifest.ActiveDocuments || got.Versions != manifest.Versions {
		return manifest, errors.New("restored corpus fingerprint/counts mismatch")
	}
	return manifest, nil
}

func importCorpus(ctx context.Context, compressed []byte, root string, manifest *CorpusManifest) (err error) {
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	hash, count := sha256.New(), &byteCounter{}
	input := io.TeeReader(io.LimitReader(gz, manifest.UncompressedBytes+1), io.MultiWriter(hash, count))
	dec := json.NewDecoder(input)
	dec.DisallowUnknownFields()
	destinationStore := store.New(root)
	documents := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry corpusEntry
		if err := dec.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode corpus: %w", err)
		}
		documents++
		if documents > manifest.Documents {
			return errors.New("archive exceeds declared document count")
		}
		if err := destinationStore.ImportDoc(ctx, entry.Path, entry.Document); err != nil {
			return fmt.Errorf("import %s: %w", entry.Path, err)
		}
	}
	if documents != manifest.Documents || count.n != manifest.UncompressedBytes || fmt.Sprintf("sha256-%x", hash.Sum(nil)) != manifest.CorpusSHA256 {
		return errors.New("corpus payload checksum/size/count mismatch")
	}
	return nil
}

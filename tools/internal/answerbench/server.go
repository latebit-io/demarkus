package answerbench

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol/store"
)

func seed(root string, fixture *Fixture) error {
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	s := store.New(root)
	for _, doc := range fixture.Documents {
		for version, body := range doc.Versions {
			if _, err := s.WriteVersion(doc.Path, version, []byte(body), doc.Metadata); err != nil {
				return fmt.Errorf("seed %s version %d: %w", doc.Path, version+1, err)
			}
		}
	}
	fixed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			return os.Chtimes(path, fixed, fixed)
		}
		return nil
	})
}

func awaitServer(ctx context.Context, host, index string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := fetch.NewClient(fetch.Options{Insecure: true})
	defer client.Close()
	var last error
	for {
		probe, stop := context.WithTimeout(ctx, 300*time.Millisecond)
		res, err := client.FetchContext(probe, host, "/index.md", "")
		stop()
		if err == nil && res.Response.Status == "ok" && res.Response.Body == index {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixture server did not start: %w (last probe: %v)", ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

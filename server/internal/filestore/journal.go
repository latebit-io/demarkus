package filestore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/latebit-io/demarkus/protocol"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// journalName is the change journal beside the documents: a dotfile, so
// listings hide it and no document can take its name (documents end in .md).
const journalName = ".changes"

// treeKey carries the store fingerprint on the header and on every appended
// block, so a reopen can tell whether the tree changed behind the journal.
const treeKey = "tree"

// journal is the durable tail of a file store's change sequence in the WATCH
// block codec: a header naming the epoch, then one event block per commit.
// It keeps one ring's worth, rewritten when torn and every ring of appends.
type journal struct {
	path   string
	epoch  string
	ring   int
	logger *slog.Logger

	file     *os.File
	tail     []changefeed.Event
	appended int
	// down is set once a rewrite failed and logged; appends keep retrying
	// the rewrite quietly until one succeeds.
	down bool
}

// openJournal restores the journal at dir. Missing, unreadable, recorded
// against another fingerprint than the tree's, or with no fingerprint to
// check, it starts a new epoch: a resumed watcher then resyncs.
func openJournal(dir, fingerprint string, ring int, logger *slog.Logger) (*journal, error) {
	j := &journal{path: filepath.Join(dir, journalName), ring: ring, logger: logger}
	recorded, clean, err := j.read()
	var reason string
	switch {
	case errors.Is(err, os.ErrNotExist):
		reason = "no change journal"
	case err != nil:
		reason = "change journal unreadable: " + err.Error()
	case fingerprint == "":
		reason = "hash index not built"
	case recorded != fingerprint:
		reason = "content changed behind the change journal"
	}
	trimmed := len(j.tail) > ring
	switch {
	case reason != "":
		j.epoch, j.tail = changefeed.NewEpoch(), nil
		logger.Info("change journal starts a new epoch", "reason", reason, "epoch", j.epoch)
		err = j.rewrite(fingerprint)
	case trimmed || !clean:
		if trimmed {
			j.tail = j.tail[len(j.tail)-ring:]
		}
		err = j.rewrite(fingerprint)
	default:
		err = j.openForAppend()
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// append records one commit and the fingerprint after it. A failure is
// logged, never surfaced: the commit stands, and the fingerprint check at
// the next open turns a missing entry into a resync.
func (j *journal) append(ev changefeed.Event, fingerprint string) {
	if j.file == nil && !j.recover(fingerprint) {
		return
	}
	block := ev.Block(j.epoch)
	block.Metadata[treeKey] = fingerprint
	if _, err := block.WriteTo(j.file); err != nil {
		j.fail("change journal append failed", err)
		return
	}
	j.tail = append(j.tail, ev)
	if len(j.tail) > j.ring {
		j.tail = j.tail[1:]
	}
	j.appended++
	if j.appended > j.ring {
		if err := j.rewrite(fingerprint); err != nil {
			j.fail("change journal compaction failed", err)
		}
	}
}

// recover retries the rewrite a failed one left half done, so an outage
// ends with the disk coming back rather than at the next restart.
func (j *journal) recover(fingerprint string) bool {
	if err := j.rewrite(fingerprint); err != nil {
		j.fail("change journal rewrite failed", err)
		return false
	}
	j.down = false
	j.logger.Info("change journal recovered", "epoch", j.epoch)
	return true
}

// fail logs one outage notice; later failures stay quiet until recovery.
func (j *journal) fail(what string, err error) {
	if j.down {
		return
	}
	j.down = true
	j.logger.Error(what+"; journal off until it recovers, watchers resync at the next restart", "error", err)
}

func (j *journal) close() error {
	if j.file == nil {
		return nil
	}
	err := errors.Join(j.file.Sync(), j.file.Close())
	j.file = nil
	return err
}

// rewrite replaces the file with the header and the retained tail, then
// reopens it for appends. fingerprint goes on the header, which is what a
// reopen checks when no event follows it.
func (j *journal) rewrite(fingerprint string) error {
	if err := j.close(); err != nil {
		return fmt.Errorf("close change journal: %w", err)
	}
	if _, err := protocolstore.WriteFileAtomic(j.path, 0o600, func(w io.Writer) error {
		buffered := bufio.NewWriter(w)
		header := protocol.WatchControl(protocol.StatusOK, protocol.Cursor{Epoch: j.epoch})
		header.Metadata[treeKey] = fingerprint
		if _, err := header.WriteTo(buffered); err != nil {
			return err
		}
		for _, ev := range j.tail {
			if _, err := ev.Block(j.epoch).WriteTo(buffered); err != nil {
				return err
			}
		}
		return buffered.Flush()
	}); err != nil {
		return fmt.Errorf("rewrite change journal: %w", err)
	}
	return j.openForAppend()
}

func (j *journal) openForAppend() error {
	file, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open change journal: %w", err)
	}
	j.file, j.appended = file, 0
	return nil
}

// read fills epoch and tail from the header and the contiguous event blocks
// after it, and returns the fingerprint last recorded. A block the codec
// refuses or a sequence out of order ends the readable prefix (clean false).
func (j *journal) read() (recorded string, clean bool, err error) {
	f, err := os.Open(j.path)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	reader := protocol.NewWatchReader(f)
	header, err := reader.Next()
	if err != nil {
		return "", false, fmt.Errorf("change journal header: %w", err)
	}
	cursor, err := header.Cursor()
	if header.Status != protocol.StatusOK || err != nil {
		return "", false, fmt.Errorf("change journal header: status %q: %w", header.Status, err)
	}
	j.epoch, recorded = cursor.Epoch, header.Metadata[treeKey]
	for {
		block, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return recorded, true, nil
		}
		if err == nil {
			err = j.add(block)
		}
		if err != nil {
			j.logger.Warn("change journal torn; keeping the readable prefix", "entries", len(j.tail), "error", err)
			return recorded, false, nil
		}
		if tree, ok := block.Metadata[treeKey]; ok {
			recorded = tree
		}
	}
}

// add appends one event block to the tail, refusing another epoch or a gap.
func (j *journal) add(block protocol.WatchBlock) error {
	wire, err := block.Event()
	if err != nil {
		return err
	}
	if wire.Cursor.Epoch != j.epoch {
		return fmt.Errorf("epoch %q in a journal of %q", wire.Cursor.Epoch, j.epoch)
	}
	if n := len(j.tail); n > 0 && wire.Cursor.Seq != j.tail[n-1].Seq+1 {
		return fmt.Errorf("sequence %d after %d", wire.Cursor.Seq, j.tail[n-1].Seq)
	}
	j.tail = append(j.tail, changefeed.EventOf(wire))
	return nil
}

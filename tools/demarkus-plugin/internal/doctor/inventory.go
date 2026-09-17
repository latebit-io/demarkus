package doctor

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/protocol"
)

// inventory lists the scope recursively, archived included, following every
// cursor. A truncating bound fails the audit: every check below depends on
// knowing which documents exist.
func (a *audit) inventory(ctx context.Context) error {
	queue := []string{a.opts.Scope}
	calls := 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if a.dirs[dir] {
			continue
		}
		a.dirs[dir] = true
		cursor, after := "", ""
		cursors := map[string]bool{}
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("inventory stopped: %w", err)
			}
			if calls >= MaxListCalls {
				return fmt.Errorf("inventory needs more than %d list calls", MaxListCalls)
			}
			calls++
			resp, err := a.store.List(ctx, dir, true, cursor)
			if err != nil {
				return fmt.Errorf("list %s: %w", dir, err)
			}
			if resp.Status == protocol.StatusNotFound && dir == a.opts.Scope {
				return fmt.Errorf("scope %s not found", dir)
			}
			if resp.Status != protocol.StatusOK {
				return fmt.Errorf("list %s returned %s", dir, resp.Status)
			}
			page, err := listing.ParsePage(dir, resp, after)
			if err != nil {
				return fmt.Errorf("list %s: %w", dir, err)
			}
			for _, bad := range page.Invalid {
				a.note("list %s returned an invalid entry %q", dir, bad)
			}
			for _, e := range page.Entries {
				if e.IsDir {
					queue = append(queue, e.Path+"/")
					continue
				}
				if len(a.docs) >= MaxDocuments {
					return fmt.Errorf("scope holds more than %d documents", MaxDocuments)
				}
				a.docs[e.Path] = &document{path: e.Path}
				a.order = append(a.order, e.Path)
			}
			after = page.LastName
			if page.Complete {
				break
			}
			if page.NextCursor == "" || cursors[page.NextCursor] {
				return fmt.Errorf("list %s did not advance its cursor", dir)
			}
			cursors[page.NextCursor] = true
			cursor = page.NextCursor
		}
	}
	return nil
}

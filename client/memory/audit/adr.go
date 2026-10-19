package audit

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/latebit-io/demarkus/client/listwalk"
)

// NextADR is the next unused decision number in dir, zero padded, over
// every page with archived records included: a retired number is never
// reused. An empty directory starts at 0001.
func NextADR(ctx context.Context, store Store, dir string) (string, error) {
	highest := 0
	w := listwalk.Walker{
		Client: storeLister{store}, IncludeArchived: true, MaxDepth: listwalk.RootOnly,
		// Subdirectories hold no records; anything else hides a number.
		OnProblem: func(p *listwalk.Problem) error {
			if p.Kind == listwalk.ProblemDepth {
				return nil
			}
			return listwalk.Abort(p)
		},
	}
	err := w.Walk(ctx, dir, func(docPath string) error {
		if m := adrFileRe.FindStringSubmatch(path.Base(docPath)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				highest = max(highest, n)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", adrNumberWidth, highest+1), nil
}

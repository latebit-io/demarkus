package retrievalbench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSession struct {
	Tools
	closed bool
}

func (s *fakeSession) Close() error { s.closed = true; return nil }

// flakyStrategy fails on the question whose id is failID.
type flakyStrategy struct{ failID string }

func (flakyStrategy) Name() string { return "flaky" }

func (s flakyStrategy) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	if q.ID == s.failID {
		// Hit alongside an error must not survive into the report.
		return Outcome{Hit: true, CallsToEvidence: 1}, errors.New("boom")
	}
	if _, err := rec.Call(ctx, "mark_lookup", map[string]any{"url": "/"}); err != nil {
		return Outcome{}, err
	}
	return Outcome{Hit: true, CallsToEvidence: 1}, nil
}

func TestRunRecordsQuestionFailureAndContinues(t *testing.T) {
	set := QuestionSet{Scope: "/", Questions: []Question{
		{ID: "a", Category: CategoryBody, Query: "x", Question: "?", ExpectedPath: "/a.md"},
		{ID: "b", Category: CategoryBody, Query: "x", Question: "?", ExpectedPath: "/b.md"},
		{ID: "c", Category: CategoryBody, Query: "x", Question: "?", ExpectedPath: "/c.md"},
	}}
	var sessions []*fakeSession
	var log strings.Builder
	cfg := Config{
		Strategy: flakyStrategy{failID: "b"},
		Counter:  wordCounter{},
		Log:      &log,
		Timeout:  time.Second,
		Open: func(context.Context) (Session, error) {
			s := &fakeSession{Tools: fakeTools{rows: []string{"/a.md"}}}
			sessions = append(sessions, s)
			return s, nil
		},
	}
	report, err := Run(context.Background(), &cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Questions) != 3 || report.Failed() != 1 {
		t.Fatalf("questions=%d failed=%d", len(report.Questions), report.Failed())
	}
	if b := report.Questions[1]; b.Error != "boom" || b.Hit || b.CallsToEvidence != 0 {
		t.Fatalf("failed question recorded as %+v", b)
	}
	if all := report.Summaries[len(report.Summaries)-1]; all.Hits != 2 || all.Questions != 3 {
		t.Fatalf("summary %+v", all)
	}
	for _, s := range sessions {
		if !s.closed {
			t.Fatal("session left open")
		}
	}
	if !strings.Contains(log.String(), "error=boom") {
		t.Fatalf("progress log lacks the error:\n%s", log.String())
	}
}

func TestRunAbortsWhenSessionCannotOpen(t *testing.T) {
	cfg := Config{
		Strategy: flakyStrategy{},
		Counter:  wordCounter{},
		Open:     func(context.Context) (Session, error) { return nil, errors.New("no binary") },
	}
	set := QuestionSet{Scope: "/", Questions: []Question{{ID: "a", Category: CategoryBody, Query: "x", Question: "?", ExpectedPath: "/a.md"}}}
	if _, err := Run(context.Background(), &cfg, set); err == nil || !strings.Contains(err.Error(), "no binary") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunTimeoutReachesStrategy(t *testing.T) {
	deadlineSeen := false
	cfg := Config{
		Strategy: strategyFunc(func(ctx context.Context, _ *Recorder, _ *Question) (Outcome, error) {
			_, deadlineSeen = ctx.Deadline()
			return Outcome{}, nil
		}),
		Counter: wordCounter{},
		Timeout: time.Minute,
		Open:    func(context.Context) (Session, error) { return &fakeSession{Tools: fakeTools{}}, nil },
	}
	set := QuestionSet{Scope: "/", Questions: []Question{{ID: "a", Category: CategoryBody, Query: "x", Question: "?", ExpectedPath: "/a.md"}}}
	if _, err := Run(context.Background(), &cfg, set); err != nil {
		t.Fatal(err)
	}
	if !deadlineSeen {
		t.Fatal("strategy ran without the per-question deadline")
	}
}

type strategyFunc func(context.Context, *Recorder, *Question) (Outcome, error)

func (strategyFunc) Name() string { return "func" }
func (f strategyFunc) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	return f(ctx, rec, q)
}

package knowledgeserver

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/latebit-io/demarkus/server/internal/classifier"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

// JudgeFactory builds the anthropic classifier kind's call for model, with an
// optional base URL; the key comes from the SDK's chain. It is injected so a
// binary without it (the soul) links no SDK.
type JudgeFactory func(model, baseURL string) func(ctx context.Context, path, body string) (bool, string, error)

// newClassify is the write path's classifier stamp for cfg, logging each
// verdict; nil when none is configured.
func newClassify(cfg knowledgeconfig.ClassifierConfig, judges JudgeFactory, logger *slog.Logger) (func(ctx context.Context, path, body string) string, error) {
	var judge classifier.Judge
	source := cfg.Model
	switch cfg.Kind {
	case "":
		return nil, nil
	case knowledgeconfig.ClassifierHTTP:
		judge = classifier.HTTP(cfg.URL, cfg.Threshold, cfg.MaxInFlight)
		if source == "" {
			source = "classifier"
			if u, err := url.Parse(cfg.URL); err == nil && u.Host != "" {
				source = u.Host
			}
		}
	case knowledgeconfig.ClassifierAnthropic:
		if judges == nil {
			return nil, errors.New("contentCheck.classifier kind anthropic is not built into this binary")
		}
		judge = judges(cfg.Model, cfg.URL)
	}
	stamper := classifier.New(judge, source, classifier.Limits{Timeout: time.Duration(cfg.Timeout), MaxBytes: cfg.MaxBytes, MaxInFlight: cfg.MaxInFlight})
	return func(ctx context.Context, path, body string) string {
		stamp := stamper.Stamp(ctx, path, body)
		if stamp != "" {
			logger.Info("classifier", "audit", true, "path", path, "verdict", stamp)
		}
		return stamp
	}, nil
}

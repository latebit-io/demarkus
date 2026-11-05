package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// HTTP is the judge for an endpoint that takes {"path","text"} and answers
// {"label","score","reason"}: a sidecar in front of a local model (Prompt
// Guard). conns sizes the idle pool to the stamper's concurrency.
func HTTP(endpoint string, threshold float64, conns int) Judge {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = conns
	h := &httpJudge{url: endpoint, threshold: threshold, client: &http.Client{Transport: transport}}
	return h.judge
}

type httpJudge struct {
	url       string
	threshold float64
	client    *http.Client
}

// benignLabels are what a classifier answers for clean text.
var benignLabels = []string{"benign", "safe", "clean", "label_0"}

// maxResponse bounds what a reply may make the server read.
const maxResponse = 64 << 10

func (h *httpJudge) judge(ctx context.Context, path, body string) (injection bool, reason string, err error) {
	payload, err := json.Marshal(map[string]string{"path": path, "text": body})
	if err != nil {
		return false, "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(payload))
	if err != nil {
		return false, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("classifier unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read to the limit below; nothing to report on close
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("classifier answered %s", resp.Status)
	}
	var got struct {
		Label  string   `json:"label"`
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&got); err != nil {
		return false, "", fmt.Errorf("malformed classifier reply: %w", err)
	}
	if got.Label == "" || got.Score == nil {
		return false, "", fmt.Errorf("malformed classifier reply: label and score are required")
	}
	label := strings.ToLower(got.Label)
	if slices.Contains(benignLabels, label) || *got.Score < h.threshold {
		return false, "", nil
	}
	if reason = got.Reason; reason == "" {
		reason = fmt.Sprintf("%s %.2f", label, *got.Score)
	}
	return true, reason, nil
}

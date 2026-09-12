package answerbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const readerPrompt = `Answer using only the frozen knowledge MCP tools. Start with lookup or /index.md; choose your own search terms and navigation. Prefer budgeted body lookup and specific sections to unnecessary full reads. Source facts may differ across revisions or regions. Do not guess missing facts.
Return only JSON: {"answer": {requested fields}, "citations": [{"field": "answer field", "url": "/doc.md/vN#section", "quote": "exact supporting source sentence"}], "abstain": false}.
Use exact source identifiers and the requested field types. Every field needs supporting citations; a field combining sources needs a citation for each. Cite an immutable version and section actually observed; quote complete supporting sentences. If the snapshot cannot answer, return {"answer":{},"citations":[],"abstain":true}.`

// Config pins the reader and limits independently of the answer keys.
type Config struct {
	OpenCode     string
	Server       string
	MCP          string
	Proxy        string
	Output       string
	Temp         string
	Model        string
	Variant      string
	Case         string
	Port         int
	Repeats      int
	Steps        int
	Timeout      time.Duration
	Log          io.Writer
	Corpus       string
	Questions    string
	Origin       string
	ReaderPolicy string
}

// RunSpec must match before comparing two runs.
type RunSpec struct {
	Suite            string            `json:"suite"`
	Model            string            `json:"model"`
	Variant          string            `json:"variant"`
	OpenCode         string            `json:"opencode_version"`
	Steps            int               `json:"steps"`
	Timeout          string            `json:"timeout"`
	Repeats          int               `json:"repeats"`
	Case             string            `json:"case,omitempty"`
	Hashes           map[string]string `json:"fixture_hashes"`
	PromptHash       string            `json:"reader_prompt_hash"`
	Port             int               `json:"port"`
	ExpectedAttempts int               `json:"expected_attempts"`
	ConfigHash       string            `json:"reader_config_hash"`
	Origin           string            `json:"origin,omitempty"`
	Documents        int               `json:"documents,omitempty"`
	Versions         int               `json:"versions,omitempty"`
	ReaderPolicy     string            `json:"reader_policy,omitempty"`
}

// Attempt retains failed answers and their spend alongside successes.
type Attempt struct {
	Task      string  `json:"task"`
	Category  string  `json:"category"`
	Repeat    int     `json:"repeat"`
	ElapsedMS float64 `json:"elapsed_ms"`
	Requests  int     `json:"protocol_requests"`
	Trace     Trace   `json:"trace"`
	Score     Score   `json:"score"`
	Error     string  `json:"error,omitempty"`
}

// Summary leaves the ratio undefined when usage is incomplete or nothing is correct.
type Summary struct {
	Attempts         int      `json:"attempts"`
	Correct          int      `json:"correct"`
	KnownTokens      int64    `json:"known_total_tokens"`
	UsageComplete    bool     `json:"usage_complete"`
	TokensPerCorrect *float64 `json:"tokens_per_correct_answer"`
	Usage            Usage    `json:"usage"`
	ModelTurns       int      `json:"model_turns"`
	ToolCalls        int      `json:"tool_calls"`
	ProtocolRequests int      `json:"protocol_requests"`
}

// Report preserves the experiment identity and every scheduled reader attempt.
type Report struct {
	Spec         RunSpec            `json:"spec"`
	Generated    time.Time          `json:"generated"`
	Binaries     map[string]string  `json:"implementation_hashes"`
	Attempts     []Attempt          `json:"attempts"`
	Summary      Summary            `json:"summary"`
	Categories   map[string]Summary `json:"categories"`
	RescoredFrom string             `json:"rescored_from_sha256,omitempty"`
	MetricsOnly  bool               `json:"metrics_only,omitempty"`
	SourceReport string             `json:"source_report_sha256,omitempty"`
	ResultTokens *int               `json:"tool_result_tokens,omitempty"`
}

// Summarize charges all observed spend, including incorrect answers.
func Summarize(attempts []Attempt) Summary {
	s := Summary{Attempts: len(attempts), UsageComplete: len(attempts) > 0}
	for i := range attempts {
		attempt := &attempts[i]
		s.KnownTokens += attempt.Trace.Usage.Tokens()
		s.Usage.Add(attempt.Trace.Usage)
		s.ModelTurns += attempt.Trace.Turns
		s.ToolCalls += len(attempt.Trace.Calls)
		s.ProtocolRequests += attempt.Requests
		s.UsageComplete = s.UsageComplete && attempt.Trace.UsageComplete && attempt.Error == ""
		if attempt.Score.Correct && attempt.Error == "" {
			s.Correct++
		}
	}
	if s.UsageComplete && s.Correct > 0 {
		ratio := float64(s.KnownTokens) / float64(s.Correct)
		s.TokensPerCorrect = &ratio
	}
	return s
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_") || strings.HasPrefix(kv, "DEMARKUS_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (cfg *Config) validate() error {
	if _, err := policyPrompt(cfg.ReaderPolicy); err != nil {
		return err
	}
	if cfg.Repeats < 1 || cfg.Steps < 1 || cfg.Timeout <= 0 || cfg.Port < 1024 || cfg.Port > 65535 || !strings.Contains(cfg.Model, "/") {
		return errors.New("positive repeats/steps/timeout, port 1024..65535, and provider/model required")
	}
	if cfg.Corpus != "" || cfg.Questions != "" || cfg.Origin != "" {
		origin, err := url.Parse(cfg.Origin)
		if cfg.Corpus == "" || cfg.Questions == "" || err != nil || origin.Scheme != "mark" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
			return errors.New("copied corpus requires -corpus, -questions, and a mark://host origin")
		}
	}
	return nil
}

// Run starts fresh readers over a frozen server and preserves partial results on error.
func Run(ctx context.Context, cfg *Config) (report Report, err error) {
	if err := cfg.validate(); err != nil {
		return report, err
	}
	f, err := LoadFixture()
	if cfg.Corpus != "" {
		f, err = LoadStoreFixture(ctx, cfg.Corpus, cfg.Questions)
	}
	if err != nil {
		return report, err
	}
	if err := f.selectTask(cfg.Case); err != nil {
		return report, err
	}
	report, err = prepareReport(ctx, cfg, &f)
	if err != nil {
		return report, err
	}
	if err := os.Mkdir(cfg.Output, 0o700); err != nil {
		return report, fmt.Errorf("create new output directory: %w", err)
	}
	defer func() {
		report.summarize()
		if err != nil || len(report.Attempts) != report.Spec.ExpectedAttempts {
			report.Summary.UsageComplete = false
			report.Summary.TokensPerCorrect = nil
		}
		err = errors.Join(err, writeJSON(filepath.Join(cfg.Output, "report.json"), report))
	}()
	work, err := os.MkdirTemp(cfg.Temp, "demarkus-answer-bench-")
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	root := filepath.Join(work, "corpus")
	if f.StoreRoot != "" {
		root = f.StoreRoot
		defer func() { err = errors.Join(err, verifyFrozenCorpus(cfg, f.Hashes["corpus"])) }()
	} else {
		if err := seed(root, &f); err != nil {
			return report, err
		}
	}
	serverLog, err := os.Create(filepath.Join(cfg.Output, "server.jsonl"))
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, serverLog.Close()) }()
	serverCtx, cancel := context.WithCancel(ctx)
	server := child(serverCtx, cfg.Server, "-root", root, "-port", fmt.Sprint(cfg.Port), "-read-only")
	server.Env = append(cleanEnv(os.Environ()), "DEMARKUS_LOG_FORMAT=json", "DEMARKUS_RATE_LIMIT=0")
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		cancel()
		return report, err
	}
	defer func() {
		cancel()
		waitErr := server.Wait()
		var exit *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exit) && !errors.Is(waitErr, context.Canceled) {
			err = errors.Join(err, fmt.Errorf("stop fixture server: %w", waitErr))
		}
	}()
	host := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	index, _ := f.Section(Evidence{Path: "/index.md", Version: f.Latest("/index.md")})
	if err := awaitServer(ctx, host, index); err != nil {
		return report, err
	}
	host = cfg.logicalHost()
	for repeat := 1; repeat <= cfg.Repeats; repeat++ {
		for _, task := range f.Tasks {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			attempt, runErr := runAttempt(ctx, cfg, &attemptInput{task: task, repeat: repeat, work: work, serverLog: serverLog})
			if runErr != nil {
				attempt.Error = runErr.Error()
				attempt.Trace.UsageComplete = false
			}
			attempt.Score = f.Score(task.ID, &attempt.Trace, host)
			report.Attempts = append(report.Attempts, attempt)
			if cfg.Log != nil {
				if _, logErr := fmt.Fprintf(cfg.Log, "%s/%d correct=%t tokens=%d complete=%t calls=%d error=%s\n", task.ID, repeat, attempt.Score.Correct, attempt.Trace.Usage.Tokens(), attempt.Trace.UsageComplete, len(attempt.Trace.Calls), attempt.Error); logErr != nil {
					return report, logErr
				}
			}
			if runErr != nil {
				return report, runErr // Preserve partial spend; don't repeat an auth/config failure.
			}
		}
	}
	return report, nil
}

func (report *Report) summarize() {
	report.Summary = Summarize(report.Attempts)
	report.Categories = make(map[string]Summary)
	groups := make(map[string][]Attempt)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		groups[attempt.Category] = append(groups[attempt.Category], *attempt)
	}
	for category, attempts := range groups {
		report.Categories[category] = Summarize(attempts)
	}
}

type attemptInput struct {
	task      Task
	repeat    int
	work      string
	serverLog *os.File
}

func runAttempt(ctx context.Context, cfg *Config, input *attemptInput) (attempt Attempt, err error) {
	task, repeat, work, serverLog := input.task, input.repeat, input.work, input.serverLog
	attempt.Task, attempt.Category, attempt.Repeat = task.ID, task.Category, repeat
	name := fmt.Sprintf("%s-%d", task.ID, repeat)
	dir := filepath.Join(work, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return attempt, err
	}
	config, err := readerConfig(cfg, dir)
	if err != nil {
		return attempt, err
	}
	// The child gets this task's question/field types, never category or rubric.
	prompt, err := taskPrompt(task)
	if err != nil {
		return attempt, err
	}
	if err := os.WriteFile(filepath.Join(cfg.Output, name+"-prompt.txt"), []byte(prompt), 0o600); err != nil {
		return attempt, err
	}
	stdout, err := os.Create(filepath.Join(cfg.Output, name+"-events.jsonl"))
	if err != nil {
		return attempt, err
	}
	defer func() { err = errors.Join(err, stdout.Close()) }()
	stderr, err := os.Create(filepath.Join(cfg.Output, name+"-stderr.txt"))
	if err != nil {
		return attempt, err
	}
	defer func() { err = errors.Join(err, stderr.Close()) }()
	startOffset, err := serverLog.Seek(0, io.SeekCurrent)
	if err != nil {
		return attempt, err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := child(ctx, cfg.OpenCode, "run", "--pure", "--format", "json", "--agent", "benchmark", "--model", cfg.Model, "--variant", cfg.Variant, "--title", "Frozen retrieval benchmark")
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(os.Environ()),
		"XDG_CONFIG_HOME="+dir, "OPENCODE_CONFIG_CONTENT="+string(config),
		"OPENCODE_DB="+filepath.Join(dir, "opencode.db"), "OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX=4096", "OPENCODE_PURE=1")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	runErr := cmd.Run()
	attempt.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	if _, err := stdout.Seek(0, io.SeekStart); err != nil {
		return attempt, errors.Join(runErr, err)
	}
	attempt.Trace, err = ParseEvents(stdout)
	if err != nil {
		return attempt, errors.Join(runErr, err)
	}
	logs, err := os.ReadFile(serverLog.Name())
	if err != nil {
		return attempt, errors.Join(runErr, err)
	}
	for line := range bytes.SplitSeq(logs[startOffset:], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Message string `json:"msg"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return attempt, errors.Join(runErr, fmt.Errorf("parse server log: %w", err))
		}
		if record.Message == "request" {
			attempt.Requests++
		}
	}
	return attempt, runErr
}

func readerConfig(cfg *Config, dir string) ([]byte, error) {
	prompt, err := policyPrompt(cfg.ReaderPolicy)
	if err != nil {
		return nil, err
	}
	host := cfg.logicalHost()
	command := []string{cfg.Proxy, "mcp", "-mcp-bin", cfg.MCP, "-host", host}
	if cfg.Corpus != "" {
		command = append(command, "-dial-address", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	}
	return json.Marshal(map[string]any{
		"$schema": "https://opencode.ai/config.json", "share": "disabled", "autoupdate": false, "snapshot": false,
		"instructions": []string{}, "permission": map[string]string{"*": "deny", "fixture_*": "allow"},
		"compaction": map[string]bool{"auto": false, "prune": false}, "formatter": false, "lsp": false,
		"default_agent": "benchmark", "agent": map[string]any{
			"benchmark": map[string]any{"mode": "primary", "prompt": prompt, "steps": cfg.Steps},
			"title":     map[string]bool{"disable": true}, "summary": map[string]bool{"disable": true},
		},
		"mcp": map[string]any{"fixture": map[string]any{
			"type": "local", "command": command,
			"environment": map[string]string{"HOME": dir, "DEMARKUS_AUTH": ""}, "timeout": 30000,
		}},
	})
}

func taskPrompt(task Task) (string, error) {
	fields, err := json.Marshal(task.Fields)
	if err != nil {
		return "", err
	}
	return task.Question + "\nAnswer fields and types: " + string(fields), nil
}

func prepareReport(ctx context.Context, cfg *Config, f *Fixture) (Report, error) {
	var report Report
	version, err := exec.CommandContext(ctx, cfg.OpenCode, "--version").Output()
	if err != nil {
		return report, fmt.Errorf("opencode version: %w", err)
	}
	if strings.TrimSpace(string(version)) != "1.18.30" {
		return report, fmt.Errorf("OpenCode 1.18.30 required for verified usage semantics; found %s", strings.TrimSpace(string(version)))
	}
	prompt, err := policyPrompt(cfg.ReaderPolicy)
	if err != nil {
		return report, err
	}
	report.Spec = RunSpec{Suite: "graph-answer-v1", Model: cfg.Model, Variant: cfg.Variant, OpenCode: strings.TrimSpace(string(version)), Steps: cfg.Steps, Timeout: cfg.Timeout.String(), Repeats: cfg.Repeats, Case: cfg.Case, Hashes: f.Hashes, PromptHash: digest([]byte(prompt)), Port: cfg.Port, ReaderPolicy: policyName(cfg.ReaderPolicy)}
	report.Spec.ExpectedAttempts = len(f.Tasks) * cfg.Repeats
	if cfg.Corpus != "" {
		report.Spec.Suite = "soul-answer-v1"
		report.Spec.Origin = cfg.Origin
		report.Spec.Documents = len(f.Documents)
		report.Spec.Versions = f.VersionCount
	}
	canonical := *cfg
	canonical.Proxy, canonical.MCP = "<runner>", "<mcp>"
	configJSON, err := readerConfig(&canonical, "<session>")
	if err != nil {
		return report, err
	}
	report.Spec.ConfigHash = digest(configJSON)
	report.Generated = time.Now().UTC()
	report.Binaries = make(map[string]string)
	for name, binary := range map[string]string{"server": cfg.Server, "mcp": cfg.MCP, "runner": cfg.Proxy} {
		raw, readErr := os.ReadFile(binary)
		if readErr != nil {
			return report, fmt.Errorf("hash %s binary: %w", name, readErr)
		}
		report.Binaries[name] = digest(raw)
	}
	return report, nil
}

func (cfg *Config) logicalHost() string {
	if cfg.Origin != "" {
		return strings.TrimSuffix(strings.TrimPrefix(cfg.Origin, "mark://"), "/")
	}
	return fmt.Sprintf("127.0.0.1:%d", cfg.Port)
}

func verifyFrozenCorpus(cfg *Config, before string) error {
	after, err := LoadStoreFixture(context.Background(), cfg.Corpus, cfg.Questions)
	if err != nil {
		return err
	}
	if after.Hashes["corpus"] != before {
		return errors.New("frozen corpus changed during run")
	}
	return nil
}

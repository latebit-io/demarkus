package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		level      string
		logLevel   string // level to log at
		msg        string
		wantOutput bool
		wantJSON   bool
	}{
		{
			name:       "text format info level",
			format:     "text",
			level:      "info",
			logLevel:   "info",
			msg:        "hello",
			wantOutput: true,
		},
		{
			name:       "json format info level",
			format:     "json",
			level:      "info",
			logLevel:   "info",
			msg:        "hello",
			wantOutput: true,
			wantJSON:   true,
		},
		{
			name:       "debug level logs debug",
			format:     "text",
			level:      "debug",
			logLevel:   "debug",
			msg:        "trace detail",
			wantOutput: true,
		},
		{
			name:       "info level filters debug",
			format:     "text",
			level:      "info",
			logLevel:   "debug",
			msg:        "filtered",
			wantOutput: false,
		},
		{
			name:       "warn level filters info",
			format:     "text",
			level:      "warn",
			logLevel:   "info",
			msg:        "filtered",
			wantOutput: false,
		},
		{
			name:       "error level filters warn",
			format:     "text",
			level:      "error",
			logLevel:   "warn",
			msg:        "filtered",
			wantOutput: false,
		},
		{
			name:       "unknown format defaults to text",
			format:     "banana",
			level:      "info",
			logLevel:   "info",
			msg:        "hello",
			wantOutput: true,
		},
		{
			name:       "unknown level defaults to info",
			format:     "text",
			level:      "banana",
			logLevel:   "debug",
			msg:        "filtered",
			wantOutput: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := New(tt.format, tt.level, &buf)

			switch tt.logLevel {
			case "debug":
				logger.Debug(tt.msg)
			case "info":
				logger.Info(tt.msg)
			case "warn":
				logger.Warn(tt.msg)
			case "error":
				logger.Error(tt.msg)
			}

			output := buf.String()
			hasOutput := strings.TrimSpace(output) != ""

			if hasOutput != tt.wantOutput {
				t.Errorf("wantOutput=%v, got output=%q", tt.wantOutput, output)
			}

			if tt.wantJSON && hasOutput {
				var m map[string]any
				if err := json.Unmarshal([]byte(output), &m); err != nil {
					t.Errorf("expected valid JSON, got: %q", output)
				}
			}
		})
	}
}

func TestNew_NilWriter(t *testing.T) {
	// Should not panic with nil writer (defaults to stderr).
	logger := New("text", "info", nil)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

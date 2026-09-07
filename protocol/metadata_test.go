package protocol

import (
	"slices"
	"testing"
)

func TestSplitTags(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a, b ,,c", []string{"a", "b", "c"}},
		{" , ", nil},
	} {
		if got := SplitTags(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("SplitTags(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsWriteSuccess(t *testing.T) {
	for status, want := range map[string]bool{StatusOK: true, StatusCreated: true, StatusConflict: false, "": false} {
		if got := IsWriteSuccess(status); got != want {
			t.Errorf("IsWriteSuccess(%q) = %v, want %v", status, got, want)
		}
	}
}

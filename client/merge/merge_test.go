package merge

import "testing"

func TestParseOnConflict(t *testing.T) {
	for raw, want := range map[string]string{"": OnConflictMerge, "  ": OnConflictMerge, "merge": OnConflictMerge, " fail ": OnConflictFail} {
		if got, err := ParseOnConflict(raw); err != nil || got != want {
			t.Errorf("ParseOnConflict(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseOnConflict("overwrite"); err == nil || err.Error() != `invalid on_conflict "overwrite": expected "merge" or "fail"` {
		t.Errorf("err = %v", err)
	}
}

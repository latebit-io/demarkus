package probe

import "testing"

// A probe that ran reports so even on a non-zero exit; one that could not
// run, or outran the budget, does not.
func TestBounded(t *testing.T) {
	if out, ran := Bounded(nil, "sh", "-c", "echo hello"); out != "hello" || !ran {
		t.Errorf("Bounded(echo) = %q, %v; want %q, true", out, ran, "hello")
	}
	if out, ran := Bounded(nil, "sh", "-c", "exit 1"); out != "" || !ran {
		t.Errorf("Bounded(exit 1) = %q, %v; want empty, true", out, ran)
	}
	if _, ran := Bounded(nil, "definitely-not-a-real-command-9f3c"); ran {
		t.Error("Bounded(missing command) reported the probe as having run")
	}
	if _, ran := Bounded(nil, "sh", "-c", "sleep 60"); ran {
		t.Error("Bounded(hanging command) reported the probe as having run")
	}
	if out, _ := Bounded([]string{"PROBE_TEST=set"}, "sh", "-c", "echo $PROBE_TEST"); out != "set" {
		t.Errorf("Bounded with env = %q, want the variable passed", out)
	}
}

package main

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadNudgeInput(t *testing.T) {
	t.Run("empty input is a zero request", func(t *testing.T) {
		in, err := readNudgeInput(strings.NewReader("  \n"))
		if err != nil || in.Event != "" {
			t.Errorf("in = %+v, err = %v", in, err)
		}
	})
	t.Run("payload decodes", func(t *testing.T) {
		in, err := readNudgeInput(strings.NewReader(`{"event":"recall"}`))
		if err != nil || in.Event != "recall" {
			t.Errorf("in = %+v, err = %v", in, err)
		}
	})
	t.Run("read failure is reported", func(t *testing.T) {
		boom := errors.New("pipe closed")
		if _, err := readNudgeInput(iotest.ErrReader(boom)); !errors.Is(err, boom) {
			t.Errorf("err = %v, want the read error", err)
		}
	})
	t.Run("bad JSON is reported", func(t *testing.T) {
		if _, err := readNudgeInput(strings.NewReader("{")); err == nil {
			t.Error("want a parse error")
		}
	})
}

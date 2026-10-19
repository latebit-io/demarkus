package release

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
)

// Binary states, kept distinct so a status report never collapses them.
const (
	binaryOK            = "ok"
	binaryMissing       = "missing"
	binaryNotExecutable = "not-executable"
	binaryOld           = "old"
	binaryBroken        = "broken"
)

// binaryState is one managed binary read against its pin.
type binaryState struct {
	name, state string
	version     string // what --version answered, "" when it did not
	detail      string // the pin it misses, or what failed
}

func (b binaryState) line() string {
	parts := []string{"binary", b.name, b.state}
	for _, part := range []string{b.version, b.detail} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

// binaryStates reads the managed binaries concurrently, each --version probe
// bounded on its own: the server and client against their pins, demarkus-token
// against the tools release.
func binaryStates() []binaryState {
	pins := []struct{ name, pin string }{{"demarkus-server", ServerVersion}, {"demarkus-mcp", ClientVersion}, {"demarkus-token", ToolsRef()}}
	states := make([]binaryState, len(pins))
	var wg sync.WaitGroup
	for i, p := range pins {
		wg.Go(func() { states[i] = readBinary(p.name, p.pin) })
	}
	wg.Wait()
	return states
}

// BinaryLines is one status report row per managed binary.
func BinaryLines() []string {
	states := binaryStates()
	lines := make([]string, 0, len(states))
	for _, b := range states {
		lines = append(lines, b.line())
	}
	return lines
}

func readBinary(name, pin string) binaryState {
	p, err := config.BinPath(name)
	if err != nil {
		return binaryState{name: name, state: binaryBroken, detail: err.Error()}
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return binaryState{name: name, state: binaryMissing}
	}
	switch refusal := procscan.ExecRefusal(p); refusal {
	case "":
	case "is not a regular executable":
		return binaryState{name: name, state: binaryNotExecutable}
	default:
		return binaryState{name: name, state: binaryBroken, detail: refusal}
	}
	v := procscan.BinaryVersionAt(p)
	switch {
	case v == "":
		return binaryState{name: name, state: binaryBroken, detail: "--version failed or timed out"}
	case v != pin:
		return binaryState{name: name, state: binaryOld, version: v, detail: "(pin " + pin + ")"}
	}
	return binaryState{name: name, state: binaryOK, version: v}
}

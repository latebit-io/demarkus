package nudge

import (
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
)

// JournalWriteCall reports whether a tool hook payload is a journal write
// (config.IsJournalWrite), so shell adapters need no JSON parsing of their own.
func JournalWriteCall(in *Input) bool {
	in.normalizeTool()
	tool, args := host.NormalizeCall(in.Tool, in.Input)
	url, _ := args["url"].(string)
	route, _ := args["route"].(string)
	return config.IsJournalWrite(tool, url, route)
}

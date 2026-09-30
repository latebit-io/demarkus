// demarkus-knowledge-server is the multi-world knowledge server over GCS;
// the runtime is knowledgeserver.Run, this main only names the binary.
package main

import (
	"fmt"
	"os"

	"github.com/latebit-io/demarkus/server/knowledgeserver"
)

var version = "dev"

func main() {
	if err := knowledgeserver.Run(os.Args[1:], version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

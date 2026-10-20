// demarkus-knowledge-server is the multi-world knowledge server over GCS;
// the runtime is knowledgeserver.Run, this main only names the binary.
package main

import (
	"fmt"
	"os"

	"github.com/latebit-io/demarkus/server/knowledgeserver"
	"github.com/latebit-io/demarkus/server/knowledgeserver/bucketworlds"
)

var version = "dev"

func main() {
	if err := knowledgeserver.Run(os.Args[1:], version, knowledgeserver.Options{Stores: bucketworlds.GCS()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

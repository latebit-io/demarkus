//go:build !unix

package broker

import "os"

type fileOwner struct {
	uid, gid int
}

// ownerOf is a no-op on platforms without unix ownership semantics.
func ownerOf(_ os.FileInfo) *fileOwner { return nil }

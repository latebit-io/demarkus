//go:build unix

package broker

import (
	"os"
	"syscall"
)

type fileOwner struct {
	uid, gid int
}

// ownerOf extracts uid/gid from a stat result; nil when the platform
// stat shape is unavailable (callers then skip ownership preservation).
func ownerOf(info os.FileInfo) *fileOwner {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &fileOwner{uid: int(st.Uid), gid: int(st.Gid)}
}

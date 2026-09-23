package versionstore

import (
	"io/fs"
	"os"
)

// fingerprint is the metadata compared before and after reading a file. Any
// difference means the file changed while it was being captured.
type fingerprint struct {
	size    int64
	mtimeNs int64
	ctimeNs int64
	inode   uint64
	mode    fs.FileMode
}

func fingerprintOf(info os.FileInfo) fingerprint {
	ctimeNs, inode := platformFileIdentity(info)
	return fingerprint{
		size:    info.Size(),
		mtimeNs: info.ModTime().UnixNano(),
		ctimeNs: ctimeNs,
		inode:   inode,
		mode:    info.Mode(),
	}
}

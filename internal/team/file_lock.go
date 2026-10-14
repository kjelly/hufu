package team

import "errors"

// errFileLockHeld reports that another open file description holds an
// exclusive file lock taken with lockExclusiveFile.
var errFileLockHeld = errors.New("file lock is held")

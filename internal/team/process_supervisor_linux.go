//go:build linux

package team

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A terminated orphan may remain a zombie until its adopter waits for it.
// Inspect the group after SIGKILL so an unreaped member cannot be mistaken
// for a writer and no live member may survive settlement.
func unixProcessGroupHasLiveMember(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] < '0' || entry.Name()[0] > '9' {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		// comm is parenthesized and may itself contain whitespace or ')'.
		end := strings.LastIndexByte(string(stat), ')')
		fields := strings.Fields(string(stat[end+1:]))
		if end < 0 || len(fields) < 3 {
			return false, fmt.Errorf("process supervisor: malformed process stat")
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}

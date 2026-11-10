//go:build linux

package team

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTerminalProcessGroupContained(t *testing.T) {
	cases := []struct {
		name      string
		readErr   error
		remaining string
		want      bool
		wantErr   error
	}{
		{name: "wrapped vanished process", readErr: syscall.ESRCH, want: true},
		{name: "wrapped missing stat", readErr: os.ErrNotExist, want: true},
		{name: "vanished process does not hide live group member", readErr: syscall.ESRCH, remaining: "125 (child) S 123 123", want: false},
		{name: "unrelated process", remaining: "125 (other) S 1 999", want: true},
		{name: "zombie group member", remaining: "125 (child) Z 123 123", want: true},
		{name: "permission failure stays closed", readErr: syscall.EACCES, wantErr: syscall.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, pid := range []string{"123", "124", "125"} {
				if err := os.Mkdir(filepath.Join(root, pid), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			read := func(path string) ([]byte, error) {
				switch filepath.Base(filepath.Dir(path)) {
				case "123":
					t.Error("waitable leader must be excluded from the scan")
				case "124":
					if tc.readErr != nil {
						return nil, &os.PathError{Op: "read", Path: path, Err: tc.readErr}
					}
				case "125":
					if tc.remaining != "" {
						return []byte(tc.remaining), nil
					}
				}
				return []byte("124 (unrelated) S 1 999"), nil
			}
			got, err := terminalProcessGroupContainedInProc(root, 123, 123, read)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("contained = %v, err %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

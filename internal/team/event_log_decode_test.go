package team

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeEventLogKeepsCanonicalBytesAndRejectsInvalidSuffix(t *testing.T) {
	root := t.TempDir()
	appendExportFixture(t, root)
	var exported bytes.Buffer
	if err := ExportEventLog(t.Context(), root, &exported); err != nil {
		t.Fatal(err)
	}
	events, err := DecodeEventLog(bytes.NewReader(exported.Bytes()))
	if err != nil || len(events) != 3 {
		t.Fatalf("events=%d error=%v", len(events), err)
	}
	if !bytes.Contains(events[1].Payload, []byte("spaced  payload")) {
		t.Fatal("payload bytes changed")
	}
	for _, input := range []string{
		exported.String() + "invalid suffix\n",
		strings.Replace(exported.String(), `"run_finished"`, `"forged_finished"`, 1),
		strings.SplitN(exported.String(), "\n", 2)[1],
	} {
		if prefix, err := DecodeEventLog(strings.NewReader(input)); err == nil || prefix != nil {
			t.Fatalf("invalid chain exposed accepted prefix: len=%d err=%v", len(prefix), err)
		}
	}
}

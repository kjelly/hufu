package paired

import (
	"strings"
	"testing"
)

func TestProgramRequiresExplicitAccountableUsage(t *testing.T) {
	const source = `{"schema_version":1,"scripts":[{"stage":"executor","case_id":"case_a","variant_slot_id":"baseline","attempt":1,"steps":[{"content":"result","usage":{"input_tokens":100,"output_tokens":20}}]}]}`
	for _, pair := range [][2]string{
		{`,"usage":{"input_tokens":100,"output_tokens":20}`, ""},
		{`"input_tokens":100`, `"input_tokens":null`},
		{`"input_tokens":100`, `"input_tokens":-1`},
		{`"input_tokens":100`, `"input_tokens":9223372036854775807`},
		{`"input_tokens":100,`, ""},
		{`"output_tokens":20`, `"output_tokens":"20"`},
		{`"output_tokens":20`, `"output_tokens":20,"unknown":0`},
		{`"attempt":1`, `"attempt":3`},
		{`"stage":"executor"`, `"stage":"unknown"`},
		{`"stage":"executor"`, `"stage":"compose"`},
		{`"schema_version":1`, `"schema_version":1,"schema_version":1`},
	} {
		if _, err := ParseProgram([]byte(strings.Replace(source, pair[0], pair[1], 1))); err == nil {
			t.Fatalf("invalid program accepted: %v", pair)
		}
	}
	program, err := ParseProgram([]byte(source))
	if err != nil || program.Scripts[0].Steps[0].Usage.InputTokens != 100 {
		t.Fatalf("valid program: %+v, %v", program, err)
	}
	program, err = ParseProgram([]byte(strings.Replace(source, `{"input_tokens":100,"output_tokens":20}`, "null", 1)))
	if err != nil || program.Scripts[0].Steps[0].Usage != nil {
		t.Fatalf("unknown usage lost: %+v, %v", program, err)
	}
}

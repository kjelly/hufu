package team

import (
	"strings"
	"testing"
)

func TestContractDataflow(t *testing.T) {
	for _, test := range []struct {
		name, code string
		change     func(*TeamSession)
	}{
		{"compatible", "", func(*TeamSession) {}},
		{"undeclared", "contract_dataflow_unverified", func(s *TeamSession) {
			s.ResultContracts["decisions.json"].fixedCollections[0].SourceTaskContracts = nil
		}},
		{"missing producer", "contract_dataflow_source_invalid", func(s *TeamSession) { s.ContractTasks = s.ContractTasks[1:] }},
		{"no producer result", "contract_dataflow_source_invalid", func(s *TeamSession) { s.ContractTasks[0].ResultContract = nil }},
		{"new producer schema", "contract_dataflow_source_rejected", func(s *TeamSession) { s.ContractTasks[0].ResultContract = &ResultContractRef{ID: "new-assets.json"} }},
		{"missing source path", "contract_dataflow_pointer_invalid", func(s *TeamSession) {
			s.ResultContracts["decisions.json"].fixedCollections[0].SourcePointer = "/absent"
		}},
		{"open source path", "contract_dataflow_pointer_unverified", func(s *TeamSession) {
			s.ResultContracts["assets.json"].schema.AdditionalProperties = nil
			s.ResultContracts["decisions.json"].fixedCollections[0].SourcePointer = "/absent"
		}},
		{"recursive reference", "contract_dataflow_pointer_unverified", func(s *TeamSession) {
			s.ResultContracts["assets.json"].schema.Properties["entries"].Ref = s.ResultContracts["assets.json"].schema.Properties["entries"]
			s.ResultContracts["assets.json"].schema.Properties["entries"].Types = nil
		}},
		{"scalar source path", "contract_dataflow_pointer_invalid", func(s *TeamSession) {
			s.ResultContracts["assets.json"].schema.Properties["entries"].Types = s.ResultContracts["assets.json"].schema.Types
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _, _, _ := fixedCollectionFixture(t, 2)
			s := c.session
			s.ContractTasks = []TaskDef{{ID: "producer", ResultContract: new(s.ResultContracts["assets.json"].ref(true))}, {ID: "consumer", ResultContract: new(s.ResultContracts["decisions.json"].ref(true))}}
			s.ResultContracts["decisions.json"].fixedCollections[0].SourceTaskContracts = []string{"producer"}
			s.ResultContracts["assets.json"].schema.AdditionalProperties = false
			test.change(s)
			findings := LintTeamContracts(s)
			if test.code == "" {
				for _, f := range findings {
					if strings.HasPrefix(f.Code, "contract_dataflow_") {
						t.Fatalf("unexpected finding: %#v", f)
					}
				}
			} else if !hasFindingCode(findings, test.code) {
				t.Fatalf("missing %s: %#v", test.code, findings)
			}
		})
	}
}

func TestFixedCollectionSourceTaskDeclarations(t *testing.T) {
	for _, test := range []struct {
		name, ids string
		valid     bool
	}{
		{"declared", `["producer"]`, true},
		{"duplicate", `["producer","producer"]`, false},
		{"empty ID", `[""]`, false},
		{"whitespace", `[" producer"]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := strings.Replace(fixedCollectionTestSchema, `"source_contracts":`, `"source_task_contracts":`+test.ids+`,"source_contracts":`, 1)
			writeResultContractSchema(t, dir, "consumer.json", raw)
			compiled, err := compileResultContractSchema(dir, "consumer.json")
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
			if err == nil && len(compiled.fixedCollections[0].SourceTaskContracts) != 1 {
				t.Fatal("source task declarations were discarded")
			}
		})
	}
}

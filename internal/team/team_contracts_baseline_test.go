package team

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

const contractAliasFixture = `- id: inspect
  agent: worker
  side_effect: none
  recovery: retry
  max_retries: 2
  execution:
    tool-sequence: [view, submit_result]
    requires-result: true
  verify: test -f result.txt
  optional: true
`

func contractManifestFixture(key string, versioned, advanced, phases bool) string {
	body := key + ":\n" + indentContractFixture(contractAliasFixture, 2)
	if advanced {
		body = "advanced:\n" + indentContractFixture(body, 2)
	}
	if phases {
		body += "workflow:\n  phases: [prepare, verify]\n"
	}
	if versioned {
		return "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: alias-fixture\nspec:\n" + indentContractFixture(body, 2)
	}
	return "name: alias-fixture\n" + body
}

func indentContractFixture(value string, spaces int) string {
	return strings.Repeat(" ", spaces) + strings.ReplaceAll(strings.TrimSuffix(value, "\n"), "\n", "\n"+strings.Repeat(" ", spaces)) + "\n"
}

// Pin full typed contracts and both binding paths before changing the public key.
// BuildCompatSnapshot alone omits execution, action and verification fields.
func TestLegacyContractCharacterization(t *testing.T) {
	for _, phases := range []bool{false, true} {
		var baseline []TaskDef
		var initialBaseline, goalBaseline []EffectiveTaskContract
		for _, format := range []struct {
			name                string
			versioned, advanced bool
		}{
			{"flat", false, false}, {"advanced", false, true}, {"v1", true, false},
		} {
			t.Run(format.name+map[bool]string{true: "_phases", false: "_phaseless"}[phases], func(t *testing.T) {
				dir := t.TempDir()
				writeTeamManifest(t, dir, contractManifestFixture("tasks", format.versioned, format.advanced, phases))
				tasks, err := loadTeamContractTasks(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 || tasks[0].ID != "inspect" || tasks[0].Verify != "test -f result.txt" {
					t.Fatalf("lost authored contract: %+v", tasks)
				}
				session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindInitialTaskContracts: true, InitialBatch: []string{"worker"}, BindTaskGoalContracts: true}}, ContractTasks: tasks}
				_, initial, err := CompileInitialTaskContracts(session, []TaskDef{{Agent: "worker", Goal: "inspect"}})
				if err != nil {
					t.Fatal(err)
				}
				_, goal, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "worker", ContractID: "inspect", Goal: "inspect"}})
				if err != nil {
					t.Fatal(err)
				}
				if baseline == nil {
					baseline, initialBaseline, goalBaseline = tasks, initial, goal
				}
				if !reflect.DeepEqual(tasks, baseline) || !reflect.DeepEqual(initial, initialBaseline) || !reflect.DeepEqual(goal, goalBaseline) {
					t.Fatal("legacy spelling changed contracts or compiled identity")
				}
			})
		}
	}
}

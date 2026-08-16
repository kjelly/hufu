package skill

import (
	"context"
	"testing"
)

type recordingSkillModelInvoker struct{ purposes []string }

func (i *recordingSkillModelInvoker) Invoke(_ context.Context, purpose, _ string) (string, error) {
	i.purposes = append(i.purposes, purpose)
	return `{"score":0.8,"reason":"generic","specific_elements":[]}`, nil
}

func TestSkillPatternDetectorUsesPurposeSpecificInvoker(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 4)
	invoker := &recordingSkillModelInvoker{}
	detector.SetModelInvoker(invoker)
	seq := &ToolSequence{Tools: []string{"view", "edit"}, Params: []string{"file", "file"}}
	if score, _, _ := detector.evaluateParamGeneralization(context.Background(), seq); score != .8 {
		t.Fatalf("generalization score = %v", score)
	}
	if len(invoker.purposes) != 1 || invoker.purposes[0] != "skill_learning" {
		t.Fatalf("invocation purposes = %#v", invoker.purposes)
	}
}

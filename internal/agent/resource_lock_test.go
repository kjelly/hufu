package agent

import (
	"strings"
	"testing"
)

func TestRequiredResourceSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    RequiredResourceSpec
		wantErr bool
	}{
		{
			name: "valid minimal",
			spec: RequiredResourceSpec{Name: "team-rules", Kind: ResourceProjectRules, Path: "AGENTS.md"},
		},
		{
			name: "valid with sha256",
			spec: RequiredResourceSpec{
				Name: "team-rules", Kind: ResourceProjectRules, Path: "AGENTS.md",
				SHA256: strings.Repeat("a", 64),
			},
		},
		{
			name:    "empty name",
			spec:    RequiredResourceSpec{Kind: ResourceSkill, Path: "x"},
			wantErr: true,
		},
		{
			name:    "unknown kind",
			spec:    RequiredResourceSpec{Name: "x", Kind: "not-a-kind", Path: "x"},
			wantErr: true,
		},
		{
			name:    "empty path",
			spec:    RequiredResourceSpec{Name: "x", Kind: ResourceSkill},
			wantErr: true,
		},
		{
			name:    "short sha256",
			spec:    RequiredResourceSpec{Name: "x", Kind: ResourceSkill, Path: "x", SHA256: "abc123"},
			wantErr: true,
		},
		{
			name:    "non-hex sha256",
			spec:    RequiredResourceSpec{Name: "x", Kind: ResourceSkill, Path: "x", SHA256: strings.Repeat("z", 64)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRequiredResourcesRejectsDuplicateNames(t *testing.T) {
	specs := []RequiredResourceSpec{
		{Name: "team-rules", Kind: ResourceProjectRules, Path: "AGENTS.md"},
		{Name: "team-rules", Kind: ResourceSkill, Path: "SKILL.md"},
	}
	if err := ValidateRequiredResources(specs); err == nil {
		t.Fatal("ValidateRequiredResources() = nil, want duplicate-name error")
	}
}

func TestValidateRequiredResourcesEmptyIsOK(t *testing.T) {
	if err := ValidateRequiredResources(nil); err != nil {
		t.Fatalf("ValidateRequiredResources(nil) = %v, want nil", err)
	}
}

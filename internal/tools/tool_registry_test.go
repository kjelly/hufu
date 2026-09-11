package tools

import (
	"slices"
	"testing"
)

func TestBuiltinToolNamesMatchConcreteRegistry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noNet    bool
		forceMCP bool
	}{
		{name: "default"},
		{name: "no net", noNet: true},
		{name: "force mcp", forceMCP: true},
		{name: "combined", noNet: true, forceMCP: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			concrete := AllTools(WithNetworkBlock(tc.noNet), WithForceMCP(tc.forceMCP))
			names := make([]string, 0, len(concrete))
			for _, tool := range concrete {
				names = append(names, tool.Info().Name)
			}
			if want := BuiltinToolNames(tc.noNet, tc.forceMCP); !slices.Equal(names, want) {
				t.Fatalf("concrete names = %v, offline registry = %v", names, want)
			}
		})
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

func TestBuildReportMDRedactsRegisteredSecret(t *testing.T) {
	const secret = "phase3-exact-report-secret-4c21"
	registry := tools.NewSecretRegistry()
	if err := registry.Register(tools.SecretRef{Name: "test.report", Source: "test", ExactValue: secret}); err != nil {
		t.Fatal(err)
	}
	utils.RegisterSecretRedactor(registry)
	report := buildReportMD(&reportData{STM: secret}, "team", "final="+secret)
	if strings.Contains(report, secret) {
		t.Fatal("report retained registered secret")
	}
}

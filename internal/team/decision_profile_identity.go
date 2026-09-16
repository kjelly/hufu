package team

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

type decisionProfileIdentity struct {
	Origin  string
	Version string
	Ref     string
	Digest  string
}

func (i decisionProfileIdentity) empty() bool {
	return i.Origin == "" && i.Version == "" && i.Ref == "" && i.Digest == ""
}

func (i decisionProfileIdentity) equal(other decisionProfileIdentity) bool {
	return i == other
}

func validateDecisionProfileIdentity(identity decisionProfileIdentity, policy DecisionPolicy, requireComplete, allowLegacy bool) error {
	if identity.empty() {
		if requireComplete {
			return fmt.Errorf("decision profile identity is incomplete")
		}
		return nil
	}
	if identity.Origin != "" {
		switch identity.Origin {
		case agent.DecisionProfileOriginBuiltin:
			if requireComplete && (identity.Version == "" || identity.Ref == "") {
				return fmt.Errorf("builtin decision profile identity requires version and ref")
			}
		case agent.DecisionProfileOriginTeamInline, agent.DecisionProfileOriginRequestInline:
			if identity.Version != "" || identity.Ref != "" {
				return fmt.Errorf("inline decision profile identity cannot carry version or ref")
			}
		case agent.DecisionProfileOriginLegacyInline:
			if !allowLegacy {
				return fmt.Errorf("legacy-inline decision profile identity is not allowed here")
			}
			if identity.Version != "" || identity.Ref != "" {
				return fmt.Errorf("legacy decision profile identity cannot carry version or ref")
			}
		default:
			return fmt.Errorf("unknown decision profile origin %q", identity.Origin)
		}
	} else if requireComplete {
		return fmt.Errorf("decision profile origin is required")
	}
	if identity.Ref != "" {
		if !strings.HasPrefix(identity.Ref, "builtin/") {
			return fmt.Errorf("decision profile ref %q is not a builtin reference", identity.Ref)
		}
		if strings.Count(identity.Ref, "@") != 1 {
			return fmt.Errorf("decision profile ref %q is not exactly versioned", identity.Ref)
		}
		prefix, version, ok := strings.Cut(identity.Ref, "@")
		if !ok || prefix == "builtin/" || version == "" {
			return fmt.Errorf("decision profile ref %q is not exactly versioned", identity.Ref)
		}
		if identity.Version != "" && version != identity.Version {
			return fmt.Errorf("decision profile ref version %q does not match %q", version, identity.Version)
		}
	}
	if identity.Digest == "" {
		if requireComplete {
			return fmt.Errorf("decision policy digest is required")
		}
		return nil
	}
	if len(identity.Digest) != len("sha256:")+64 || !strings.HasPrefix(identity.Digest, "sha256:") {
		return fmt.Errorf("decision policy digest has invalid format")
	}
	hexPart := strings.TrimPrefix(identity.Digest, "sha256:")
	if strings.ToLower(hexPart) != hexPart {
		return fmt.Errorf("decision policy digest must use lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("decision policy digest has invalid hexadecimal: %w", err)
	}
	want, err := agent.DecisionPolicyDigest(policy)
	if err != nil {
		return fmt.Errorf("compute decision policy digest: %w", err)
	}
	if identity.Digest != want {
		return fmt.Errorf("decision policy digest mismatch")
	}
	return nil
}

func materializeDirectDecisionRequest(req DecisionRequest) (DecisionRequest, error) {
	normalized, err := agent.NormalizeDecisionPolicy(req.Policy)
	if err != nil {
		return DecisionRequest{}, err
	}
	identity := req.profileIdentity()
	if identity.empty() {
		identity.Origin = agent.DecisionProfileOriginRequestInline
	} else if identity.Origin == agent.DecisionProfileOriginLegacyInline && req.AdmissionInputDigest == "" {
		return DecisionRequest{}, fmt.Errorf("legacy-inline decision profile identity requires a durable admission")
	}
	// A v1 admission is the sole compatibility case where the new envelope
	// must retain the exact raw policy bytes. Its digest still uses the
	// normalized copy, so omitted semantic defaults compare consistently.
	if req.AdmissionInputDigest == "" {
		req.Policy = normalized
	}
	digest, err := agent.DecisionPolicyDigest(normalized)
	if err != nil {
		return DecisionRequest{}, err
	}
	if identity.Digest == "" && identity.Origin == agent.DecisionProfileOriginRequestInline && identity.Version == "" && identity.Ref == "" {
		identity.Digest = digest
	}
	req.setProfileIdentity(identity)
	if err := validateDecisionProfileIdentity(identity, req.Policy, true, req.AdmissionInputDigest != ""); err != nil {
		return DecisionRequest{}, err
	}
	return req, nil
}

func (r DecisionRequest) profileIdentity() decisionProfileIdentity {
	return decisionProfileIdentity{Origin: r.ProfileOrigin, Version: r.ProfileVersion, Ref: r.ProfileRef, Digest: r.PolicyDigest}
}

func (r *DecisionRequest) setProfileIdentity(identity decisionProfileIdentity) {
	r.ProfileOrigin, r.ProfileVersion = identity.Origin, identity.Version
	r.ProfileRef, r.PolicyDigest = identity.Ref, identity.Digest
}

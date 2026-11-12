package taskdistill

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// SchemaIssue contains only contract-owned names, JSON types and counts.
// Validator messages can contain rejected values and unknown property names;
// never publish or replay those messages as model repair feedback.
type SchemaIssue struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

const maxSchemaIssues = 4

func schemaViolation(cause error) *OutputViolation {
	issue := &OutputViolation{Code: "schema_violation", Path: "$", cause: cause}
	validation, ok := errors.AsType[*jsonschema.ValidationError](cause)
	if !ok {
		return issue
	}
	var collect func(*jsonschema.ValidationError)
	collect = func(current *jsonschema.ValidationError) {
		if len(issue.SchemaIssues) >= maxSchemaIssues {
			return
		}
		if len(current.Causes) != 0 {
			for _, child := range current.Causes {
				collect(child)
			}
			return
		}
		rule, detail := schemaConstraint(current.ErrorKind)
		issue.SchemaIssues = append(issue.SchemaIssues, SchemaIssue{
			Path: schemaInstancePath(current.InstanceLocation), Rule: rule, Detail: detail,
		})
	}
	collect(validation)
	if len(issue.SchemaIssues) > 0 {
		issue.Path = issue.SchemaIssues[0].Path
	}
	return issue
}

func schemaConstraint(constraint jsonschema.ErrorKind) (string, string) {
	switch k := constraint.(type) {
	case *kind.Type:
		return "type", "expected=" + strings.Join(k.Want, "|") + ", got=" + k.Got
	case *kind.Required:
		var names []string
		for _, name := range k.Missing {
			if schemaDiagnosticField(name) {
				names = append(names, name)
			}
		}
		return "required", "missing=" + strings.Join(names, ",")
	case *kind.AdditionalProperties:
		return "additionalProperties", "undeclared fields are forbidden"
	case *kind.Enum:
		return "enum", "use a value declared in the schema"
	case *kind.MinLength:
		return "minLength", fmt.Sprintf("limit=%d, got=%d", k.Want, k.Got)
	case *kind.MaxLength:
		return "maxLength", fmt.Sprintf("limit=%d, got=%d", k.Want, k.Got)
	case *kind.MinItems:
		return "minItems", fmt.Sprintf("limit=%d, got=%d", k.Want, k.Got)
	case *kind.MaxItems:
		return "maxItems", fmt.Sprintf("limit=%d, got=%d", k.Want, k.Got)
	case *kind.UniqueItems:
		return "uniqueItems", "duplicate array entries are forbidden"
	default:
		return "schema", "value violates the declared contract"
	}
}

func schemaInstancePath(parts []string) string {
	path := "$"
	for _, part := range parts {
		if schemaDiagnosticField(part) {
			if path == "$" {
				path = part
			} else {
				path += "." + part
			}
		} else if index, err := strconv.Atoi(part); err == nil && index >= 0 {
			path += fmt.Sprintf("[%d]", index)
		} else {
			break
		}
	}
	return path
}

func schemaDiagnosticField(name string) bool {
	return name == "texts" || schemaField(name)
}

func schemaField(name string) bool {
	switch name {
	case "decisions", "knowledge", "source_id", "action", "reason", "text", "source_ids":
		return true
	default:
		return false
	}
}

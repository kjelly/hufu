package team

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

const maxExecutionPolicyDifferences = 24

// Diagnostics compare the same historical shape used by admission. They never
// migrate the snapshot or consult current source files to reconstruct old data.
func (c *Coordinator) executionPolicySnapshotDriftError(frozen *ExecutionPolicySnapshot) error {
	current, err := c.executionPolicySnapshotForComparison(frozen)
	if err != nil {
		return fmt.Errorf("describe execution policy snapshot drift: %w", err)
	}
	diff := executionPolicyDifferences(frozen, normalizeUnusedExecutionEnvironment(frozen, current))
	return fmt.Errorf("execution policy snapshot drift detected (event_store=%s current=%s)\n%s\nRestore the original execution settings to resume, or use --new to archive the old session and start fresh", frozen.ConfigurationHash, current.ConfigurationHash, diff)
}

type executionPolicyDiff struct {
	changes []string
	omitted int
}

func executionPolicyDifferences(frozen, current *ExecutionPolicySnapshot) string {
	diff := &executionPolicyDiff{}
	diff.compare(reflect.ValueOf(frozen).Elem(), reflect.ValueOf(current).Elem(), "")
	var out strings.Builder
	out.WriteString("Changed execution policy fields:")
	for _, change := range diff.changes {
		out.WriteString("\n  - " + change)
	}
	if diff.omitted > 0 {
		fmt.Fprintf(&out, "\n  ... %d additional changes omitted", diff.omitted)
	}
	return out.String()
}

func (d *executionPolicyDiff) add(path, detail string) {
	if len(d.changes) < maxExecutionPolicyDifferences {
		d.changes = append(d.changes, path+": "+detail)
	} else {
		d.omitted++
	}
}

// Only field names, bounded/redacted entry identities, and numeric/boolean
// values are displayed. Arbitrary string values, including digests, stay hidden.
// Hash-only fields can identify a policy category, not its underlying change.
func (d *executionPolicyDiff) compare(before, after reflect.Value, path string) {
	if reflect.DeepEqual(before.Interface(), after.Interface()) {
		return
	}
	switch before.Kind() {
	case reflect.Pointer:
		if before.IsNil() {
			d.add(path, "added")
		} else if after.IsNil() {
			d.add(path, "removed")
		} else {
			d.compare(before.Elem(), after.Elem(), path)
		}
	case reflect.Struct:
		for i := range before.NumField() {
			field := before.Type().Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" || name == "-" || name == "configuration_hash" {
				continue
			}
			if before.Field(i).Kind() == reflect.Slice && before.Field(i).Len() == 0 && after.Field(i).Len() == 0 && strings.Contains(options, "omitempty") {
				continue
			}
			fieldPath := name
			if path != "" {
				fieldPath = path + "." + name
			}
			d.compare(before.Field(i), after.Field(i), fieldPath)
		}
	case reflect.Slice:
		d.compareEntries(before, after, path)
	case reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64:
		d.add(path, fmt.Sprintf("%v -> %v", before.Interface(), after.Interface()))
	default:
		detail := "changed"
		if strings.HasSuffix(path, "hash") || strings.HasSuffix(path, "sha256") || strings.HasSuffix(path, "digest") {
			detail = "fingerprint changed (underlying values are not retained in the snapshot)"
		}
		d.add(path, detail)
	}
}

func (d *executionPolicyDiff) compareEntries(before, after reflect.Value, path string) {
	priorChanges := len(d.changes) + d.omitted
	oldEntries, newEntries := policyDiffEntries(before), policyDiffEntries(after)
	keys := make(map[string]struct{}, len(oldEntries)+len(newEntries))
	for key := range oldEntries {
		keys[key] = struct{}{}
	}
	for key := range newEntries {
		keys[key] = struct{}{}
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		oldEntry, hadOld := oldEntries[key]
		newEntry, hasNew := newEntries[key]
		entryPath := path + "[" + policyDiffLabel(key) + "]"
		switch {
		case !hadOld:
			d.add(entryPath, "added")
		case !hasNew:
			d.add(entryPath, "removed")
		default:
			d.compare(oldEntry, newEntry, entryPath)
		}
	}
	if len(d.changes)+d.omitted == priorChanges {
		// The canonical hash also distinguishes entry order and null from [].
		d.add(path, "entry order or empty representation changed")
	}
}

func policyDiffEntries(entries reflect.Value) map[string]reflect.Value {
	result := make(map[string]reflect.Value, entries.Len())
	for i := range entries.Len() {
		entry := entries.Index(i)
		identity := policyDiffEntryIdentity(entry, i)
		// Retain every entry even when an identity is repeated. Model routes
		// can name the same model on more than one backend.
		key := identity
		for occurrence := 1; result[key].IsValid(); occurrence++ {
			key = fmt.Sprintf("%s#%d", identity, occurrence)
		}
		result[key] = entry
	}
	return result
}

func policyDiffEntryIdentity(entry reflect.Value, index int) string {
	if entry.Kind() == reflect.Struct {
		for _, name := range []string{"Owner", "Model", "Agent", "Name", "Capability", "Server", "Backend", "ID"} {
			field := entry.FieldByName(name)
			if field.IsValid() && field.Kind() == reflect.String {
				identity := strings.ToLower(name) + "=" + field.String()
				if name == "Model" {
					identity += ",backend=" + entry.FieldByName("Backend").String() + ",provider_key=" + entry.FieldByName("ProviderKey").String()
				}
				return identity
			}
		}
	}
	return strconv.Itoa(index)
}

func policyDiffLabel(value string) string {
	value = utils.RedactSecrets(value)
	label := []rune(value)
	if len(label) > 120 {
		value = string(label[:120]) + "..."
	}
	return strconv.QuoteToASCII(value)
}

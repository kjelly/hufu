package main

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
)

type testShard struct {
	Index int
	Count int
}

func (shard testShard) validate() error {
	if shard.Count < 1 || shard.Index < 0 || shard.Index >= shard.Count {
		return fmt.Errorf("shard index %d must be in [0, %d), with a positive shard count", shard.Index, shard.Count)
	}
	return nil
}

// Assign each package/name pair once, independently of discovery order or
// unrelated test additions. FNV-1a is a stable partition, not a timing estimate.
func (shard testShard) contains(pkg, name string) bool {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(pkg + "/" + name))
	return hash.Sum64()%uint64(shard.Count) == uint64(shard.Index)
}

func (shard testShard) inventory(selected []suite) []suite {
	if shard.Count == 1 {
		return selected
	}
	filtered := make([]suite, 0, len(selected))
	for _, entry := range selected {
		pick := func(names []string) []string {
			var chosen []string
			for _, name := range names {
				if shard.contains(entry.Package, name) {
					chosen = append(chosen, name)
				}
			}
			return chosen
		}
		filtered = append(filtered, suite{Package: entry.Package, Short: pick(entry.Short), Integration: pick(entry.Integration)})
	}
	return filtered
}

func (shard testShard) commands(layer string, selected []suite, extra []string) ([][]string, error) {
	if err := shard.validate(); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"short", "integration", "full"}, layer) {
		return nil, fmt.Errorf("invalid layer %q", layer)
	}
	if shard.Count == 1 {
		return testCommands(layer, selected, extra)
	}
	if layer == "integration" && !slices.ContainsFunc(selected, func(entry suite) bool { return len(entry.Integration) > 0 }) {
		return nil, fmt.Errorf("no integration tests selected")
	}
	base := append([]string{"test", "-p", "2", "-count=1", "-timeout", "20m"}, extra...)
	if layer == "short" {
		base = append(base, "-short")
	}
	names := make(map[string][]string, len(selected))
	for _, entry := range shard.inventory(selected) {
		switch layer {
		case "short":
			names[entry.Package] = entry.Short
		case "integration":
			names[entry.Package] = entry.Integration
		case "full":
			names[entry.Package] = append(slices.Clone(entry.Short), entry.Integration...)
		default:
			return nil, fmt.Errorf("invalid layer %q", layer)
		}
	}
	// Unfiltered short/full runs also execute examples and fuzz seeds. Keep
	// those on shard zero, including packages with no assigned Test function.
	auxiliary := shard.Index == 0 && layer != "integration"
	return matchingTestCommands(base, selected, names, auxiliary), nil
}

// Combine package invocations only when the union filter cannot select a test
// assigned to another layer or shard in any participating package.
func matchingTestCommands(base []string, selected []suite, names map[string][]string, auxiliary bool) [][]string {
	var active []suite
	var union []string
	for _, entry := range selected {
		if len(names[entry.Package]) > 0 || auxiliary {
			active = append(active, entry)
			union = append(union, names[entry.Package]...)
		}
	}
	if len(active) == 0 {
		return nil
	}
	slices.Sort(union)
	union = slices.Compact(union)
	collision := false
	for _, entry := range active {
		for _, name := range append(slices.Clone(entry.Short), entry.Integration...) {
			_, inUnion := slices.BinarySearch(union, name)
			if inUnion && !slices.Contains(names[entry.Package], name) {
				collision = true
			}
		}
	}
	pattern := func(chosen []string) string {
		result := exactPattern(chosen)
		if auxiliary {
			result = strings.TrimSuffix(result, ")$")
			if len(chosen) > 0 {
				result += "|"
			}
			result += "Example.*|Fuzz.*)$"
		}
		return result
	}
	if !collision {
		command := append(slices.Clone(base), "-run", pattern(union))
		for _, entry := range active {
			command = append(command, entry.Package)
		}
		return [][]string{command}
	}
	var commands [][]string
	for _, entry := range active {
		commands = append(commands, append(slices.Clone(base), "-run", pattern(names[entry.Package]), entry.Package))
	}
	return commands
}

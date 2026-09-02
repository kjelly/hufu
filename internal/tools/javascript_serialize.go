//go:build linux || darwin
// +build linux darwin

package tools

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/dop251/goja"
)

const maxJSExportDepth = 64

var (
	errJSCyclicResult   = errors.New("cyclic structure cannot be serialized to JSON")
	errJSMaxDepth       = errors.New("value exceeds maximum serialization depth")
	errJSUnsupportedVal = errors.New("value cannot be serialized to JSON")
)

// exportJSONCompatible converts a goja.Value into a JSON-compatible Go value
// (nil, bool, number, string, []interface{}, map[string]interface{}).
//
// It deliberately does not use goja.Value.Export() for objects/arrays: Export
// recurses through object properties itself, and a self-referencing object
// (x.self = x) would recurse it forever into a fatal, unrecoverable stack
// overflow rather than a catchable error. Walking the tree here, with a
// visited-set keyed by object identity, lets a real cycle fail cleanly with
// errJSCyclicResult instead of crashing the process.
func exportJSONCompatible(v goja.Value) (interface{}, error) {
	return exportJSValue(v, make(map[*goja.Object]bool), 0)
}

func exportJSValue(v goja.Value, visited map[*goja.Object]bool, depth int) (interface{}, error) {
	if depth > maxJSExportDepth {
		return nil, errJSMaxDepth
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}

	obj, isObj := v.(*goja.Object)
	if !isObj {
		return exportJSPrimitive(v)
	}

	if _, isFn := goja.AssertFunction(v); isFn {
		return nil, fmt.Errorf("%w: function values are not JSON-serializable", errJSUnsupportedVal)
	}

	if visited[obj] {
		return nil, errJSCyclicResult
	}
	visited[obj] = true
	defer delete(visited, obj)

	switch obj.ClassName() {
	case "Array":
		return exportJSArray(obj, visited, depth)
	case "Object":
		return exportJSObject(obj, visited, depth)
	default:
		return nil, fmt.Errorf("%w: %s values are not JSON-serializable", errJSUnsupportedVal, obj.ClassName())
	}
}

func exportJSPrimitive(v goja.Value) (interface{}, error) {
	switch exported := v.Export().(type) {
	case float64:
		if math.IsNaN(exported) || math.IsInf(exported, 0) {
			return nil, fmt.Errorf("%w: non-finite number %v", errJSUnsupportedVal, exported)
		}
		return exported, nil
	case int64, bool, string:
		return exported, nil
	default:
		return nil, fmt.Errorf("%w: unsupported primitive type %T", errJSUnsupportedVal, exported)
	}
}

func exportJSArray(obj *goja.Object, visited map[*goja.Object]bool, depth int) (interface{}, error) {
	length := obj.Get("length")
	var n int64
	if length != nil {
		n = length.ToInteger()
	}
	arr := make([]interface{}, 0, n)
	for i := int64(0); i < n; i++ {
		elem := obj.Get(strconv.FormatInt(i, 10))
		val, err := exportJSValue(elem, visited, depth+1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	return arr, nil
}

func exportJSObject(obj *goja.Object, visited map[*goja.Object]bool, depth int) (interface{}, error) {
	m := make(map[string]interface{})
	for _, key := range obj.Keys() {
		raw := obj.Get(key)
		if raw == nil || goja.IsUndefined(raw) {
			continue // matches JSON.stringify: keys with an undefined value are omitted
		}
		val, err := exportJSValue(raw, visited, depth+1)
		if err != nil {
			return nil, err
		}
		m[key] = val
	}
	return m, nil
}

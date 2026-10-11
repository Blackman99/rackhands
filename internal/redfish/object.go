package redfish

import (
	"fmt"
)

// Object is a decoded Redfish resource with forgiving accessors: BMC
// firmware is inconsistent about types and presence, so lookups never panic.
type Object map[string]any

// Get walks a dotted path ("Status.Health", "Oem.Hpe.AggregateHealthStatus").
// Keys that themselves contain dots ("@odata.id",
// "ResetType@Redfish.AllowableValues") are matched whole first.
func (o Object) Get(path string) any {
	return walk(map[string]any(o), path)
}

func walk(m map[string]any, path string) any {
	if v, ok := m[path]; ok {
		return v
	}
	for i := 0; i < len(path); i++ {
		if path[i] != '.' {
			continue
		}
		if v, ok := m[path[:i]]; ok {
			if sub, ok := asMap(v); ok {
				if r := walk(sub, path[i+1:]); r != nil {
					return r
				}
			}
		}
	}
	return nil
}

func asMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case Object:
		return map[string]any(t), true
	}
	return nil, false
}

// Obj returns a nested object (empty if missing).
func (o Object) Obj(path string) Object {
	if m, ok := asMap(o.Get(path)); ok {
		return Object(m)
	}
	return Object{}
}

// Str returns a string field, formatting numbers; "" if absent.
func (o Object) Str(path string) string {
	switch v := o.Get(path).(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case bool:
		return fmt.Sprintf("%t", v)
	}
	return ""
}

// Num returns a numeric field and whether it was present.
func (o Object) Num(path string) (float64, bool) {
	v, ok := o.Get(path).(float64)
	return v, ok
}

// List returns an array of objects.
func (o Object) List(path string) []Object {
	arr, _ := o.Get(path).([]any)
	out := make([]Object, 0, len(arr))
	for _, e := range arr {
		if m, ok := asMap(e); ok {
			out = append(out, Object(m))
		}
	}
	return out
}

// Strings returns an array of strings.
func (o Object) Strings(path string) []string {
	arr, _ := o.Get(path).([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Ref returns the @odata.id of a link field ("Thermal" -> "/redfish/v1/...").
func (o Object) Ref(path string) string { return o.Obj(path).Str("@odata.id") }

// Refs returns the @odata.id of every element of a link array.
func (o Object) Refs(path string) []string {
	var out []string
	for _, m := range o.List(path) {
		if id := m.Str("@odata.id"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ID returns @odata.id.
func (o Object) ID() string { return o.Str("@odata.id") }

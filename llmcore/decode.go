package llmcore

// Str, Int, and Float decode one field from the map[string]any that RunRoute
// and RunExpand return (raw JSON tool-call output). Every caller of those two
// functions needs this same unwrapping — promoted here instead of leaving it
// duplicated per caller.
func Str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func Int(m map[string]any, k string) (int, bool) {
	switch v := m[k].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func Float(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

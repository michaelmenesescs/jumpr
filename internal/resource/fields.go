package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Str formats the field value of id deterministically. Missing fields and nil
// values yield "". Floats use strconv.FormatFloat(v, 'f', -1, 64); sparkline
// values are comma-separated; unknown types fall back to fmt.Sprint.
func (r Resource) Str(id string) string {
	return formatValue(r.Fields[id])
}

// Int reports the field value of id as an integer. int, int32 and int64 are
// accepted, and float64/float32 only when the value is integral and in range.
func (r Resource) Int(id string) (int64, bool) {
	switch v := r.Fields[id].(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float32:
		return integral(float64(v))
	case float64:
		return integral(v)
	}
	return 0, false
}

// Float reports the field value of id as a float. int, int32, int64, float32
// and float64 are accepted.
func (r Resource) Float(id string) (float64, bool) {
	switch v := r.Fields[id].(type) {
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

// Time reports the field value of id as a timestamp.
func (r Resource) Time(id string) (time.Time, bool) {
	t, ok := r.Fields[id].(time.Time)
	return t, ok
}

// Compare orders two field values for sorting. Values of the same numeric kind
// compare numerically, strings lexically, times chronologically, durations by
// length and bools false before true. nil sorts last. Values whose types do not
// match compare by their Str rendering.
func Compare(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return 1
		default:
			return -1
		}
	}

	if na, ok := numberValue(a); ok {
		if nb, ok := numberValue(b); ok {
			if na.isInt && nb.isInt {
				return compareInt(na.i, nb.i)
			}
			return compareFloat(na.float(), nb.float())
		}
	}

	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return strings.Compare(sa, sb)
		}
	}

	if ta, ok := a.(time.Time); ok {
		if tb, ok := b.(time.Time); ok {
			switch {
			case ta.Before(tb):
				return -1
			case ta.After(tb):
				return 1
			default:
				return 0
			}
		}
	}

	if da, ok := a.(time.Duration); ok {
		if db, ok := b.(time.Duration); ok {
			return compareInt(int64(da), int64(db))
		}
	}

	if ba, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			switch {
			case ba == bb:
				return 0
			case !ba:
				return -1
			default:
				return 1
			}
		}
	}

	if sa, ok := a.([]float64); ok {
		if sb, ok := b.([]float64); ok {
			return compareSeries(sa, sb)
		}
	}

	return strings.Compare(formatValue(a), formatValue(b))
}

// formatValue renders one field value deterministically.
func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case time.Duration:
		return x.String()
	case []float64:
		parts := make([]string, len(x))
		for i, f := range x {
			parts[i] = strconv.FormatFloat(f, 'f', -1, 64)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(v)
	}
}

// integral converts an integral, in-range float to int64.
func integral(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	// float64(1<<63) is exactly 2^63, one past the largest int64.
	if f < -float64(1<<63) || f >= float64(1<<63) {
		return 0, false
	}
	return int64(f), true
}

// number is a field value that participates in numeric ordering.
type number struct {
	isInt bool
	i     int64
	f     float64
}

func (n number) float() float64 {
	if n.isInt {
		return float64(n.i)
	}
	return n.f
}

// numberValue classifies a value as an integer or a float.
func numberValue(v any) (number, bool) {
	switch x := v.(type) {
	case int:
		return number{isInt: true, i: int64(x)}, true
	case int32:
		return number{isInt: true, i: int64(x)}, true
	case int64:
		return number{isInt: true, i: x}, true
	case float32:
		return number{f: float64(x)}, true
	case float64:
		return number{f: x}, true
	}
	return number{}, false
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareSeries orders two sparkline series element by element, shorter first
// when the common prefix is equal.
func compareSeries(a, b []float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := compareFloat(a[i], b[i]); c != 0 {
			return c
		}
	}
	return compareInt(int64(len(a)), int64(len(b)))
}

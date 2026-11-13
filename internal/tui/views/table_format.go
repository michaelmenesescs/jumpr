package views

import (
	"strconv"
	"time"

	"sitrep/internal/render"
	"sitrep/internal/resource"
)

// cellText renders one field of row as plain text, following the column's
// kind. Missing values render as an em dash; the caller decides the style.
func cellText(c resource.Column, row resource.Resource, now time.Time, width int) string {
	if c.Kind == resource.ColStatus {
		return statusText(row.Status)
	}
	v := row.Fields[c.ID]
	if v == nil {
		return "—"
	}
	switch c.Kind {
	case resource.ColNumber:
		if n, ok := valueInt(v); ok {
			return render.Int(n)
		}
		if f, ok := valueFloat(v); ok {
			return strconv.FormatFloat(f, 'f', 2, 64)
		}
	case resource.ColBytes:
		if n, ok := valueInt(v); ok {
			return render.Bytes(n)
		}
	case resource.ColPercent:
		if f, ok := valueFloat(v); ok {
			return render.Percent(f)
		}
	case resource.ColDuration:
		if d, ok := v.(time.Duration); ok {
			return render.Duration(d)
		}
	case resource.ColTime:
		if ts, ok := v.(time.Time); ok {
			if !ts.IsZero() && now.Sub(ts) > 24*time.Hour {
				return ts.Format("Jan 02")
			}
			return ts.Format("15:04:05")
		}
	case resource.ColSpark:
		if vals, ok := v.([]float64); ok {
			return render.Spark(vals, width)
		}
	}
	return row.Str(c.ID)
}

// valueInt reads an integer field value.
func valueInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

// valueFloat reads a numeric field value as a float.
func valueFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// statusText renders a resource status as one word.
func statusText(s resource.Status) string {
	switch s {
	case resource.StatusOK:
		return "ok"
	case resource.StatusWarn:
		return "warn"
	case resource.StatusCrit:
		return "crit"
	case resource.StatusNA:
		return "n/a"
	default:
		return "unknown"
	}
}

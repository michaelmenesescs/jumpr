# Task 009 — Sparklines, bars, human-readable numbers

- **Milestone:** M0
- **Depends on:** 002
- **Parallel group:** M0-A (with 003, 004, 008)
- **Status:** done

## Goal
Small pure formatters used by tables and overviews.

## Context
SPEC §13 (sparklines), mockup host overview (pressure bars, `1,481 identical`).

## Files
- create `internal/render/spark.go`, `internal/render/human.go`, `internal/render/spark_test.go`,
  `internal/render/human_test.go`

## Requirements
1. `func Spark(vals []float64, width int) string` — block characters `▁▂▃▄▅▆▇█`, scaled to the max value (max 0 →
   all `▁`); if `len(vals) > width` downsample by averaging buckets; if fewer, left-pad with spaces. `NaN` → space.
2. `func Bar(fraction float64, width int) string` — `█` for the filled part, `·` for the rest; fraction clamped to
   0..1.
3. `func Int(n int64) string` — thousands separators: `1,512`.
4. `func Bytes(n int64) string` — IEC: `512 B`, `1.5 KiB`, `31.2 GiB` (one decimal above bytes).
5. `func Duration(d time.Duration) string` — `41d 3h`, `2h 5m`, `38m`, `12s`, `0s`; two units at most.
6. `func Age(t, now time.Time) string` — `Duration(now.Sub(t))`; zero time → `—`.
7. `func Percent(f float64) string` — `71%` (no decimals at ≥ 10, one decimal below: `3.2%`).

## Out of scope
Braille line charts (task 101).

## Acceptance
```
go test ./internal/render/ -run 'Spark|Bar|Int|Bytes|Duration|Age|Percent' -v
```

## Pitfalls
- `Width(Spark(vals, w))` must equal `w` for every input, including empty `vals`.
- Negative numbers in `Int` keep the sign: `-1,000`.

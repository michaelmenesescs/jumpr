# Task 040 — Linux parsers: host overview

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-D (with 039, 041, 042, 043, 049)
- **Status:** done

## Goal
Pure parsers for the host overview probe sections.

## Context
SPEC §8.1 (`host`). BUILD-PLAN §9 (parser tests), D8.

## Files
- create `internal/backend/linux/parse_overview.go`, `internal/backend/linux/parse_overview_test.go`
- create `testdata/linux/overview/rocky9/{loadavg,meminfo,psi,os-release,uptime}.txt` (from the samples below)

## Requirements
Each parser takes the section's stdout as `string` and returns a typed struct and an error (`errs.Parse` with the
probe name). Unknown lines are ignored. Never panic on short or garbage input.
1. `ParseLoadavg("0.52 0.41 0.30 2/413 99123")` → `Load{L1, L5, L15 float64; Running, Total int}`.
2. `ParseMeminfo` → `Mem{TotalKB, AvailableKB, FreeKB, BuffersKB, CachedKB, SwapTotalKB, SwapFreeKB int64}`;
   `func (m Mem) UsedPercent() float64` = `(Total − Available) / Total × 100` (0 when Total is 0).
   Lines look like `MemTotal:       32653820 kB`.
3. `ParsePSI` — input is the concatenation produced by the probe:
   ```
   == cpu
   some avg10=1.23 avg60=0.80 avg300=0.40 total=123456
   == memory
   some avg10=0.00 avg60=0.00 avg300=0.00 total=0
   full avg10=0.00 avg60=0.00 avg300=0.00 total=0
   == io
   some avg10=3.20 avg60=1.10 avg300=0.50 total=987654
   full avg10=2.90 avg60=0.90 avg300=0.40 total=876543
   ```
   → `PSI{CPU, Mem, IO PSILine}` with `PSILine{SomeAvg10, FullAvg10 float64; Present bool}`. A resource with no
   lines (kernel without PSI) → `Present = false`, no error.
4. `ParseOSRelease` → `OS{ID, VersionID, PrettyName string}` (values may be quoted with `"` or `'`).
5. `ParseUptime("3561234.56 28012345.11")` → `time.Duration`.
6. `ParseSystemState("degraded\n")` → string, one of `running|degraded|starting|maintenance|stopping|offline|
   unknown` (anything else → `unknown`).
7. `ParseNproc("8\n")` → int.

## Out of scope
Running probes. `/proc/stat` CPU percentages.

## Acceptance
```
go test ./internal/backend/linux/ -run 'Loadavg|Meminfo|PSI|OSRelease|Uptime|SystemState|Nproc' -v
# each parser: golden file → expected struct; empty input → errs.Parse (except PSI: all Present=false);
# a garbage table (binary bytes, half lines) must not panic
```

## Pitfalls
- `/proc/meminfo` on old kernels lacks `MemAvailable`; fall back to `MemFree + Buffers + Cached`.
- Use `strings.Fields`, not fixed column offsets.

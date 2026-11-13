package render

import (
	"math"
	"strconv"
	"time"
)

// Int renders n with thousands separators: 1,512. Negative numbers keep their
// sign: -1,000.
func Int(n int64) string {
	neg := n < 0
	var mag uint64
	if neg {
		// -(n+1)+1 avoids overflowing on math.MinInt64.
		mag = uint64(-(n + 1)) + 1
	} else {
		mag = uint64(n)
	}

	digits := strconv.FormatUint(mag, 10)
	groups := (len(digits) + 2) / 3
	var b []byte
	b = make([]byte, 0, len(digits)+groups-1)
	head := len(digits) % 3
	if head == 0 {
		head = 3
	}
	b = append(b, digits[:head]...)
	for i := head; i < len(digits); i += 3 {
		b = append(b, ',')
		b = append(b, digits[i:i+3]...)
	}

	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// Bytes renders a byte count in IEC units with one decimal above bytes:
// 512 B, 1.5 KiB, 31.2 GiB.
func Bytes(n int64) string {
	const unit = 1024
	if n > -unit && n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	neg := n < 0
	var mag uint64
	if neg {
		mag = uint64(-(n + 1)) + 1
	} else {
		mag = uint64(n)
	}
	f := float64(mag) / unit
	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}

	s := strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
	if neg {
		return "-" + s
	}
	return s
}

// Duration renders a duration with at most two units: 41d 3h, 2h 5m, 38m,
// 12s, 0s. Sub-second durations render as 0s.
func Duration(d time.Duration) string {
	neg := d < 0
	var ns uint64
	if neg {
		// -(d+1)+1 avoids overflowing on the most negative duration.
		ns = uint64(-(int64(d) + 1)) + 1
	} else {
		ns = uint64(d)
	}
	secs := ns / uint64(time.Second)

	sizes := [...]uint64{86400, 3600, 60, 1}
	names := [...]string{"d", "h", "m", "s"}

	i := 0
	for i < len(sizes) && secs < sizes[i] {
		i++
	}
	if i == len(sizes) {
		return "0s"
	}
	val := secs / sizes[i]
	rem := secs % sizes[i]

	out := strconv.FormatUint(val, 10) + names[i]
	if i < len(sizes)-1 {
		if next := rem / sizes[i+1]; next > 0 {
			out += " " + strconv.FormatUint(next, 10) + names[i+1]
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

// Age renders how long ago t was relative to now, using Duration's format. A
// zero t renders as "—".
func Age(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return Duration(now.Sub(t))
}

// AgeWith is Age with the active set's placeholder: a zero t renders as
// set.Empty, everything else exactly as Age does.
func AgeWith(set GlyphSet, t, now time.Time) string {
	if !t.IsZero() {
		return Age(t, now)
	}
	if set.Empty != 0 {
		return string(set.Empty)
	}
	return Age(t, now)
}

// Percent renders a percentage: no decimals at 10 and above, one decimal
// below: 71%, 3.2%.
func Percent(f float64) string {
	if math.Abs(f) >= 10 {
		return strconv.FormatFloat(f, 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + "%"
}

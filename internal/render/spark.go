package render

import (
	"math"
	"strings"
)

// sparkBlocks is the ascending ramp Spark uses, from lowest to highest value.
const sparkBlocks = "▁▂▃▄▅▆▇█"

// Spark renders vals as a sparkline of exactly width cells. Values are scaled
// against the largest one: a zero value becomes the lowest block and the peak
// becomes the highest. A non-positive peak renders every value as the lowest
// block. NaN values render as a space. When len(vals) exceeds width the series
// is downsampled by averaging equal buckets; when it is shorter the result is
// left-padded with spaces. width <= 0 returns "".
func Spark(vals []float64, width int) string {
	if width <= 0 {
		return ""
	}
	buckets := downsample(vals, width)

	peak := 0.0
	for _, v := range buckets {
		if !math.IsNaN(v) && v > peak {
			peak = v
		}
	}

	blocks := []rune(sparkBlocks)
	last := len(blocks) - 1

	var b strings.Builder
	b.Grow(width)
	for i := len(buckets); i < width; i++ {
		b.WriteByte(' ')
	}
	for _, v := range buckets {
		if math.IsNaN(v) {
			b.WriteByte(' ')
			continue
		}
		if peak <= 0 {
			b.WriteRune(blocks[0])
			continue
		}
		idx := int(v / peak * float64(last))
		if idx < 0 {
			idx = 0
		}
		if idx > last {
			idx = last
		}
		b.WriteRune(blocks[idx])
	}
	return b.String()
}

// downsample reduces vals to exactly width buckets by averaging, or returns
// vals untouched when it already fits. NaN entries are skipped; a bucket made
// entirely of NaN stays NaN.
func downsample(vals []float64, width int) []float64 {
	n := len(vals)
	if n <= width {
		return vals
	}
	out := make([]float64, width)
	for i := range out {
		lo := i * n / width
		hi := (i + 1) * n / width
		sum := 0.0
		count := 0
		for _, v := range vals[lo:hi] {
			if math.IsNaN(v) {
				continue
			}
			sum += v
			count++
		}
		if count == 0 {
			out[i] = math.NaN()
		} else {
			out[i] = sum / float64(count)
		}
	}
	return out
}

// Bar renders a fraction as a width-cell bar: filled, then empty. fraction is
// clamped to 0..1 and NaN counts as 0. width <= 0 returns "".
func Bar(fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	if math.IsNaN(fraction) || fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(math.Round(fraction * float64(width)))
	if filled > width {
		filled = width
	}

	var b strings.Builder
	b.Grow(width * 3)
	for i := 0; i < filled; i++ {
		b.WriteRune('█')
	}
	for i := filled; i < width; i++ {
		b.WriteRune('·')
	}
	return b.String()
}

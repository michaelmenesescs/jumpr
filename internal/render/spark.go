package render

import "math"

// Spark renders vals as a sparkline of exactly width cells. Values are scaled
// against the largest one: a zero value becomes the lowest block and the peak
// becomes the highest. A non-positive peak renders every value as the lowest
// block. NaN values render as a space. When len(vals) exceeds width the series
// is downsampled by averaging equal buckets; when it is shorter the result is
// left-padded with spaces. width <= 0 returns "".
//
// Spark draws with the unicode set; use SparkWith for the active set.
func Spark(vals []float64, width int) string {
	return SparkWith(unicodeGlyphs(), vals, width)
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
//
// Bar draws with the unicode set; use BarWith for the active set.
func Bar(fraction float64, width int) string {
	return BarWith(unicodeGlyphs(), fraction, width)
}

package main

// resample24to16 downsamples 24 kHz mono PCM (int16) to 16 kHz using linear
// interpolation. The ratio is exactly 3:2, so every 3 input samples produce
// 2 output samples.
func resample24to16(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	// output length = floor(len(in) * 2 / 3)
	outLen := len(in) * 2 / 3
	out := make([]int16, outLen)

	for i := 0; i < outLen; i++ {
		// src position in input (fractional): i * 1.5
		srcPos := i * 3 // fixed-point ×2: actual = srcPos / 2
		idx := srcPos / 2
		frac := srcPos % 2 // 0 or 1 → 0.0 or 0.5

		if idx+1 < len(in) && frac != 0 {
			// linear interpolation at 0.5
			out[i] = int16((int32(in[idx]) + int32(in[idx+1])) / 2)
		} else if idx < len(in) {
			out[i] = in[idx]
		}
	}
	return out
}

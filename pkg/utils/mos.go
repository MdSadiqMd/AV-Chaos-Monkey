package utils

import "math"

// CalculateMOS calculates Mean Opinion Score using ITU-T G.107 E-model
// Returns value between 1.0 (bad) and 4.5 (excellent)
func CalculateMOS(packetLoss, jitterMs, rttMs float64) float64 {
	r0 := 93.2
	delay := rttMs / 2

	id := 0.024 * delay
	if delay > 177.3 {
		id = 0.024*delay + 0.11*(delay-177.3)*(1.0-math.Exp(-(delay-177.3)/100.0))
	}

	ie := 0.0
	if packetLoss > 0 {
		ie = 30.0 * math.Log(1.0+15.0*packetLoss/100.0)
	}
	if jitterMs > 10 {
		ie += (jitterMs - 10) * 0.5
	}

	r := r0 - id - ie
	if r < 0 {
		r = 0
	}
	if r > 100 {
		r = 100
	}

	mos := 1.0 + 0.035*r + r*(r-60)*(100-r)*7e-6
	if mos < 1.0 {
		mos = 1.0
	}
	if mos > 4.5 {
		mos = 4.5
	}
	return mos
}

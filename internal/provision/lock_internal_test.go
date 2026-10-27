package provision

import (
	"testing"
	"time"
)

func TestLockDelayBacksOffWithJitterUpToAQuarterSecond(t *testing.T) {
	for n := 0; n < 30; n++ {
		upper := lockRetryMin
		for range n {
			upper = time.Duration(float64(upper) * 1.6)
			if upper >= lockRetryMax {
				upper = lockRetryMax
				break
			}
		}
		seen := map[time.Duration]bool{}
		for range 200 {
			d := lockDelay(n)
			if d < upper/2-1 || d > upper {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", n, d, upper/2, upper)
			}
			seen[d] = true
		}
		if len(seen) < 20 {
			t.Fatalf("attempt %d: %d distinct delays in 200 draws: no jitter", n, len(seen))
		}
	}
	if lockDelay(0) > lockRetryMin || lockDelay(50) < lockRetryMax/2 || lockDelay(50) > lockRetryMax {
		t.Error("the delay does not grow from 20 ms to 250 ms")
	}
}

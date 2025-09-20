package timeutil

import (
	"math/rand"
	"time"
)

func CalcJitter(base time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(base / 10))) // 0-10%
}

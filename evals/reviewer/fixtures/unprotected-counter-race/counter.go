package racefix

// Counter is a shared in-memory tally. Add is called from multiple goroutines
// in the worker pool without any other synchronization.
type Counter struct {
	n int
}

func (c *Counter) Add(delta int) {
	c.n += delta
}

func (c *Counter) Value() int {
	return c.n
}

package supervisor

import "time"

type Backoff struct {
	min     time.Duration
	max     time.Duration
	current time.Duration
}

func NewBackoff(min, max time.Duration) *Backoff {
	return &Backoff{min: min, max: max, current: min}
}

func (b *Backoff) Current() time.Duration {
	return b.current
}

func (b *Backoff) Failure() time.Duration {
	delay := b.current
	next := b.current * 2
	if next > b.max {
		next = b.max
	}
	b.current = next
	return delay
}

func (b *Backoff) Stable() {
	b.current = b.min
}

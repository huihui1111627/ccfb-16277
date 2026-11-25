package quota

// ring is a bounded FIFO buffer. Pushing past the capacity drops the oldest
// entry and reports how many were dropped, so a busy refusal log costs O(1)
// per record instead of shifting the whole slice.
type ring[T any] struct {
	buf  []T
	head int // index of the oldest entry
	n    int // number of live entries
}

func (r *ring[T]) push(v T, capacity int) (dropped int64) {
	if capacity <= 0 {
		return 1
	}
	if r.buf == nil {
		r.buf = make([]T, capacity)
	}
	if r.n < len(r.buf) && r.n < capacity {
		r.buf[(r.head+r.n)%len(r.buf)] = v
		r.n++
		return 0
	}
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	return 1
}

func (r *ring[T]) len() int { return r.n }

// oldest returns up to max of the oldest entries. max <= 0 returns all.
func (r *ring[T]) oldest(max int) []T {
	if r.n == 0 {
		return nil
	}
	count := r.n
	if max > 0 && max < count {
		count = max
	}
	start := r.n - count
	out := make([]T, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, r.buf[(r.head+start+i)%len(r.buf)])
	}
	return out
}

// newest returns up to max of the newest entries, newest first.
func (r *ring[T]) newest(max int) []T {
	if r.n == 0 {
		return nil
	}
	count := r.n
	if max > 0 && max < count {
		count = max
	}
	out := make([]T, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, r.buf[(r.head+r.n-1-i)%len(r.buf)])
	}
	return out
}

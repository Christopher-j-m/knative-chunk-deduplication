package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// breaker trips open after a number of consecutive cache failures and stays open
// for a cooldown, after which it half-opens to let traffic probe the cache again.
type breaker struct {
	threshold int
	cooldown  time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown}
}

func (b *breaker) open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.openUntil)
}

func (b *breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.cooldown)
	}
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

type populateJob struct {
	digest string
	data   []byte
}

// populatePool runs cache writes on a fixed set of workers behind a bounded
// queue. When the queue is full a submission is dropped, so populating the cache
// never blocks the serving path.
type populatePool struct {
	jobs   chan populateJob
	work   func(digest string, data []byte)
	wg     sync.WaitGroup
	active atomic.Int64

	mu      sync.Mutex
	stopped bool
}

func newPopulatePool(workers, queue int, work func(digest string, data []byte)) *populatePool {
	p := &populatePool{jobs: make(chan populateJob, queue), work: work}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for job := range p.jobs {
				p.active.Add(1)
				p.work(job.digest, job.data)
				p.active.Add(-1)
			}
		}()
	}
	return p
}

func (p *populatePool) submit(digest string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	select {
	case p.jobs <- populateJob{digest: digest, data: data}:
	default:
	}
}

// drain blocks until the queue is empty and all running jobs have returned.
func (p *populatePool) drain() {
	for len(p.jobs) > 0 || p.active.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
}

func (p *populatePool) stop() {
	p.mu.Lock()
	p.stopped = true
	close(p.jobs)
	p.mu.Unlock()
	p.wg.Wait()
}

package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// Controls at how many consecutive cache failures (threshold) - NOT MISSES - the proxy requests the remote registry directly
// and after what duration (cooldown & concrete deadline in openUntil) the proxy can try to send requests to the cluster cache again
type breaker struct {
	mutex     sync.Mutex
	threshold int           // Amount of subsequent cache failures before re-routing to registry
	cooldown  time.Duration // Duration (s) for the re-routing to registry
	openUntil time.Time     // Duration of re-routing set as timestamp
	failures  int           // Subsequent cache failures
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown}
}

// Returns if the re-routing to the registry is active currently (due to cache failures)
func (failureBreaker *breaker) open() bool {
	failureBreaker.mutex.Lock()
	defer failureBreaker.mutex.Unlock()
	return time.Now().Before(failureBreaker.openUntil)
}

// Record a recently occured cache failure
// Blocks the cache if this new failure matches the allowed threshold
func (failureBreaker *breaker) recordFailure() {
	failureBreaker.mutex.Lock()
	defer failureBreaker.mutex.Unlock()
	failureBreaker.failures++
	if failureBreaker.failures >= failureBreaker.threshold {
		failureBreaker.openUntil = time.Now().Add(failureBreaker.cooldown)
	}
}

// Record a recently successful cache request
// This resets the failure counter for the threshold and any active cache blocking/re-routing
func (failureBreaker *breaker) recordSuccess() {
	failureBreaker.mutex.Lock()
	defer failureBreaker.mutex.Unlock()
	failureBreaker.failures = 0
	failureBreaker.openUntil = time.Time{}
}

// An item of a chunk that needs to be uploaded to the cluster cache
type populateJob struct {
	digest string
	data   []byte
}

// Cache writes with a worker pool and a queue with capacity
// If the queue capacity is reached, any new jobs are dropped
// => Since the proxy works in-memory, we dont want to slow down
// the fetch-requests by growing the queue until the proxy runs out of memory
type populatePool struct {
	jobs      chan populateJob                 // queue
	work      func(digest string, data []byte) // worker func to upload chunks to cache
	waitGroup sync.WaitGroup                   // Wait group that is used to wait for all workers to finish during proxy shutdown
	active    atomic.Int64                     // amount of currently running uploads => used for drain

	mutex   sync.Mutex
	stopped bool // Running state => true means further submit calls are ignored, since we dont want to panic if the jobs chan is closed
}

func newPopulatePool(workers, queue int, work func(digest string, data []byte)) *populatePool {
	pool := &populatePool{jobs: make(chan populateJob, queue), work: work}
	for i := 0; i < workers; i++ {
		pool.waitGroup.Add(1)
		go func() {
			defer pool.waitGroup.Done()
			for job := range pool.jobs {
				pool.active.Add(1)
				pool.work(job.digest, job.data)
				pool.active.Add(-1)
			}
		}()
	}
	return pool
}

// Add a new upload job to the queue
func (pool *populatePool) submit(digest string, data []byte) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	if pool.stopped {
		return
	}

	select {
	case pool.jobs <- populateJob{digest: digest, data: data}:
	default:
	}
}

// Blocks until the queue is empty, meaning all jobs have returned
func (pool *populatePool) drain() {
	for len(pool.jobs) > 0 || pool.active.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
}

// Sets the state to stopped, meaning no new jobs are accepted
// Also we wait for all running workers to finish their running and queued jobs
func (pool *populatePool) stop() {
	pool.mutex.Lock()
	pool.stopped = true
	close(pool.jobs)
	pool.mutex.Unlock()
	pool.waitGroup.Wait()
}

/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodecontroller

import (
	"container/heap"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/util"
)

// TimedValue is a value that should be processed at a designated time.
type TimedValue struct {
	Value string
	Added time.Time
	Next  time.Time
}

// now is used to test time
var now func() time.Time = time.Now

// TimedQueue is a priority heap where the lowest Next is at the front of the queue
type TimedQueue []*TimedValue

func (h TimedQueue) Len() int           { return len(h) }
func (h TimedQueue) Less(i, j int) bool { return h[i].Next.Before(h[j].Next) }
func (h TimedQueue) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *TimedQueue) Push(x interface{}) {
	*h = append(*h, x.(*TimedValue))
}

func (h *TimedQueue) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// A FIFO queue which additionally guarantees that any element can be added only once until
// it is removed.
type UniqueQueue struct {
	lock  sync.Mutex
	queue TimedQueue
	set   util.StringSet
}

// Adds a new value to the queue if it wasn't added before, or was explicitly removed by the
// Remove call. Returns true if new value was added.
func (q *UniqueQueue) Add(value TimedValue) bool {
	q.lock.Lock()
	defer q.lock.Unlock()

	if q.set.Has(value.Value) {
		return false
	}
	heap.Push(&q.queue, &value)
	q.set.Insert(value.Value)
	return true
}

// Removes the value from the queue, so Get() call won't return it, and allow subsequent addition
// of the given value. If the value is not present does nothing and returns false.
func (q *UniqueQueue) Remove(value string) bool {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.set.Delete(value)
	for i, val := range q.queue {
		if val.Value == value {
			if i > 0 && i < len(q.queue)-1 {
				q.queue = append(q.queue[0:i], q.queue[i+1:len(q.queue)]...)
			} else if i > 0 {
				q.queue = q.queue[0 : len(q.queue)-1]
			} else {
				q.queue = q.queue[1:len(q.queue)]
			}
			return true
		}
	}
	return false
}

// Returns the oldest added value that wasn't returned yet.
func (q *UniqueQueue) Get() (TimedValue, bool) {
	q.lock.Lock()
	defer q.lock.Unlock()
	if len(q.queue) == 0 {
		return TimedValue{}, false
	}
	result := q.queue.Pop().(*TimedValue)
	q.set.Delete(result.Value)
	return *result, true
}

// RateLimitedTimedQueue is a unique item priority queue ordered by the expected next time
// of execution. It is also rate limited.
type RateLimitedTimedQueue struct {
	queue   UniqueQueue
	limiter util.RateLimiter
	leak    bool
}

// Creates new queue which will use given RateLimiter to oversee execution. If leak is true,
// items which are rate limited will be leakped. Otherwise, rate limited items will be requeued.
func NewRateLimitedTimedQueue(limiter util.RateLimiter, leak bool) *RateLimitedTimedQueue {
	return &RateLimitedTimedQueue{
		queue: UniqueQueue{
			queue: TimedQueue{},
			set:   util.NewStringSet(),
		},
		limiter: limiter,
		leak:    leak,
	}
}

// ActionFunc takes a timed value and returns false if the item must be retried, with an optional
// time.Duration if some minimum wait interval should be used.
type ActionFunc func(TimedValue) (bool, time.Duration)

// Try processes the queue. Ends prematurely if RateLimiter forbids an action and leak is true.
// Otherwise, requeues the item to be processed. Each value is processed once if fn returns true,
// otherwise it is added back to the queue. The returned remaining is used to identify the minimum
// time to execute the next item in the queue.
func (q *RateLimitedTimedQueue) Try(fn ActionFunc) {
	val, ok := q.queue.Get()
	for ok {
		// rate limit the queue checking
		if q.leak {
			if !q.limiter.CanAccept() {
				break
			}
		} else {
			q.limiter.Accept()
		}

		now := now()
		if now.Before(val.Next) {
			q.queue.Add(val)
			val, ok = q.queue.Get()
			// we do not sleep here because other values may be added at the front of the queue
			continue
		}

		if ok, wait := fn(val); !ok {
			val.Next = now.Add(wait + 1)
			q.queue.Add(val)
		}
		val, ok = q.queue.Get()
	}
}

// Adds value to the queue to be processed. Won't add the same value a second time if it was already
// added and not removed.
func (q *RateLimitedTimedQueue) Add(value string) bool {
	now := now()
	return q.queue.Add(TimedValue{
		Value: value,
		Added: now,
		Next:  now,
	})
}

// Removes Node from the Evictor. The Node won't be processed until added again.
func (q *RateLimitedTimedQueue) Remove(value string) bool {
	return q.queue.Remove(value)
}

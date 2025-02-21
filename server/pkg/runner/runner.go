package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/emirpasic/gods/maps/linkedhashmap"
	"mayfly-go/pkg/logx"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrExist    = errors.New("job already exists")
	ErrFinished = errors.New("job already finished")
)

type JobKey = string
type RunJobFunc[T Job] func(ctx context.Context, job T)
type NextJobFunc[T Job] func() (T, bool)
type RunnableJobFunc[T Job] func(job T, next NextJobFunc[T]) bool
type ScheduleJobFunc[T Job] func(job T) (deadline time.Time, err error)

type JobStatus int

const (
	JobUnknown JobStatus = iota
	JobDelaying
	JobWaiting
	JobRunning
)

type Job interface {
	GetKey() JobKey
	Update(job Job)
}

type iterator[T Job] struct {
	index int
	data  []*wrapper[T]
	zero  T
}

func (iter *iterator[T]) Begin() {
	iter.index = -1
}

func (iter *iterator[T]) Next() (T, bool) {
	if iter.index >= len(iter.data)-1 {
		return iter.zero, false
	}
	iter.index++
	return iter.data[iter.index].job, true
}

type array[T Job] struct {
	size int
	data []*wrapper[T]
}

func newArray[T Job](size int) *array[T] {
	return &array[T]{
		size: size,
		data: make([]*wrapper[T], 0, size),
	}
}

func (a *array[T]) Iterator() *iterator[T] {
	return &iterator[T]{
		index: -1,
		data:  a.data,
	}
}

func (a *array[T]) Full() bool {
	return len(a.data) >= a.size
}

func (a *array[T]) Append(job *wrapper[T]) bool {
	if len(a.data) >= a.size {
		return false
	}
	a.data = append(a.data, job)
	return true
}

func (a *array[T]) Get(key JobKey) (*wrapper[T], bool) {
	for _, job := range a.data {
		if key == job.GetKey() {
			return job, true
		}
	}
	return nil, false
}

func (a *array[T]) Remove(key JobKey) {
	length := len(a.data)
	for i, elm := range a.data {
		if key == elm.GetKey() {
			a.data[i], a.data[length-1] = a.data[length-1], nil
			a.data = a.data[:length-1]
			return
		}
	}
}

type Runner[T Job] struct {
	maxRunning  int
	waiting     *linkedhashmap.Map
	running     *array[T]
	runJob      RunJobFunc[T]
	runnableJob RunnableJobFunc[T]
	scheduleJob ScheduleJobFunc[T]
	mutex       sync.Mutex
	wg          sync.WaitGroup
	context     context.Context
	cancel      context.CancelFunc
	zero        T
	signal      chan struct{}
	all         map[JobKey]*wrapper[T]
	delayQueue  *DelayQueue[*wrapper[T]]
}

type Opt[T Job] func(runner *Runner[T])

func WithRunnableJob[T Job](runnableJob RunnableJobFunc[T]) Opt[T] {
	return func(runner *Runner[T]) {
		runner.runnableJob = runnableJob
	}
}

func WithScheduleJob[T Job](scheduleJob ScheduleJobFunc[T]) Opt[T] {
	return func(runner *Runner[T]) {
		runner.scheduleJob = scheduleJob
	}
}

func NewRunner[T Job](maxRunning int, runJob RunJobFunc[T], opts ...Opt[T]) *Runner[T] {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &Runner[T]{
		maxRunning: maxRunning,
		all:        make(map[string]*wrapper[T], maxRunning),
		waiting:    linkedhashmap.New(),
		running:    newArray[T](maxRunning),
		context:    ctx,
		cancel:     cancel,
		signal:     make(chan struct{}, 1),
		delayQueue: NewDelayQueue[*wrapper[T]](0),
	}
	runner.runJob = runJob
	for _, opt := range opts {
		opt(runner)
	}
	if runner.runnableJob == nil {
		runner.runnableJob = func(job T, _ NextJobFunc[T]) bool {
			return true
		}
	}

	runner.wg.Add(maxRunning + 1)
	for i := 0; i < maxRunning; i++ {
		go runner.run()
	}
	go func() {
		defer runner.wg.Done()
		for runner.context.Err() == nil {
			wrap, ok := runner.delayQueue.Dequeue(ctx)
			if !ok {
				continue
			}
			runner.mutex.Lock()
			if old, ok := runner.all[wrap.key]; !ok || wrap != old {
				runner.mutex.Unlock()
				continue
			}
			runner.waiting.Put(wrap.key, wrap)
			wrap.status = JobWaiting
			runner.trigger()
			runner.mutex.Unlock()
		}
	}()
	return runner
}

func (r *Runner[T]) Close() {
	r.cancel()
	r.wg.Wait()
}

func (r *Runner[T]) run() {
	defer r.wg.Done()

	for r.context.Err() == nil {
		select {
		case <-r.signal:
			wrap, ok := r.pickRunnableJob()
			if !ok {
				continue
			}
			r.doRun(wrap)
			r.afterRun(wrap)
		case <-r.context.Done():
		}
	}
}

func (r *Runner[T]) doRun(wrap *wrapper[T]) {
	defer func() {
		if err := recover(); err != nil {
			logx.Error(fmt.Sprintf("failed to run job: %v", err))
		}
	}()

	r.runJob(r.context, wrap.job)
}

func (r *Runner[T]) afterRun(wrap *wrapper[T]) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.running.Remove(wrap.key)
	delete(r.all, wrap.key)
	wrap.status = JobUnknown
	r.trigger()
	if wrap.removed {
		return
	}
	deadline, err := r.doScheduleJob(wrap.job, true)
	if err != nil {
		return
	}
	_ = r.schedule(r.context, deadline, wrap.job)
}

func (r *Runner[T]) doScheduleJob(job T, finished bool) (time.Time, error) {
	if r.scheduleJob == nil {
		if finished {
			return time.Time{}, ErrFinished
		}
		return time.Now(), nil
	}
	return r.scheduleJob(job)
}

func (r *Runner[T]) pickRunnableJob() (*wrapper[T], bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	iter := r.running.Iterator()
	var runnable *wrapper[T]
	ok := r.waiting.Any(func(key interface{}, value interface{}) bool {
		wrap := value.(*wrapper[T])
		iter.Begin()
		if r.runnableJob(wrap.job, iter.Next) {
			if r.running.Full() {
				return false
			}
			r.waiting.Remove(key)
			r.running.Append(wrap)
			wrap.status = JobRunning
			if !r.running.Full() && !r.waiting.Empty() {
				r.trigger()
			}
			runnable = wrap
			return true
		}
		return false
	})
	if !ok {
		return nil, false
	}
	return runnable, true
}

func (r *Runner[T]) schedule(ctx context.Context, deadline time.Time, job T) error {
	wrap := newWrapper(job)
	wrap.deadline = deadline
	if wrap.deadline.After(time.Now()) {
		r.delayQueue.Enqueue(ctx, wrap)
		wrap.status = JobDelaying
	} else {
		r.waiting.Put(wrap.key, wrap)
		wrap.status = JobWaiting
		r.trigger()
	}
	r.all[wrap.key] = wrap
	return nil
}

func (r *Runner[T]) Add(ctx context.Context, job T) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if _, ok := r.all[job.GetKey()]; ok {
		return ErrExist
	}
	deadline, err := r.doScheduleJob(job, false)
	if err != nil {
		return err
	}
	return r.schedule(ctx, deadline, job)
}

func (r *Runner[T]) UpdateOrAdd(ctx context.Context, job T) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	wrap, ok := r.all[job.GetKey()]
	if ok {
		wrap.job.Update(job)
		switch wrap.status {
		case JobDelaying:
			r.delayQueue.Remove(ctx, wrap.key)
			delete(r.all, wrap.key)
		case JobWaiting:
			r.waiting.Remove(wrap.key)
			delete(r.all, wrap.key)
		case JobRunning:
			return nil
		default:
		}
	}
	deadline, err := r.doScheduleJob(job, false)
	if err != nil {
		return err
	}
	return r.schedule(ctx, deadline, wrap.job)
}

func (r *Runner[T]) StartNow(ctx context.Context, job T) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if wrap, ok := r.all[job.GetKey()]; ok {
		switch wrap.status {
		case JobDelaying:
			r.delayQueue.Remove(ctx, wrap.key)
			delete(r.all, wrap.key)
		case JobWaiting, JobRunning:
			return nil
		default:
		}
	}
	return r.schedule(ctx, time.Now(), job)
}

func (r *Runner[T]) trigger() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

func (r *Runner[T]) Remove(ctx context.Context, key JobKey) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	wrap, ok := r.all[key]
	if !ok {
		return ErrNotFound
	}
	switch wrap.status {
	case JobDelaying:
		r.delayQueue.Remove(ctx, key)
		delete(r.all, key)
	case JobWaiting:
		r.waiting.Remove(key)
		delete(r.all, key)
	case JobRunning:
		// 统一标记为 removed, 待任务执行完成后再删除
		wrap.removed = true
	default:
	}
	return nil
}

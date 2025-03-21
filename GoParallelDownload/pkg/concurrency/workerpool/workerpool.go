package workerpool

import (
	"sync"

	"github.com/gammazero/deque"
)

type WorkerPool struct {
	maxWorkers   int
	taskQueue    chan func()
	workerQueue  chan func()
	stopSignal   chan struct{}
	stoppedChan  chan struct{}
	waitingQueue deque.Deque[func()]
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	wait         bool
}

func NewWorkerPool(maxWorkers int) *WorkerPool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	pool := &WorkerPool{
		maxWorkers:  maxWorkers,
		taskQueue:   make(chan func()),
		workerQueue: make(chan func()),
		stopSignal:  make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}
	go pool.dispatch()
	return pool
}

func (p *WorkerPool) Submit(task func()) {
	if task != nil {
		p.taskQueue <- task
	}
}

func (p *WorkerPool) StopWait() {
	p.stop(true)
}

func (p *WorkerPool) dispatch() {
	defer close(p.stoppedChan)
	var workerCount int
	var wg sync.WaitGroup

	for task := range p.taskQueue {
		select {
		case p.workerQueue <- task:
		default:
			if workerCount < p.maxWorkers {
				wg.Add(1)
				go worker(task, p.workerQueue, &wg)
				workerCount++
			} else {
				p.waitingQueue.PushBack(task)
			}
		}
		for p.waitingQueue.Len() > 0 {
			task := p.waitingQueue.PopFront()
			p.workerQueue <- task
		}
	}

	for workerCount > 0 {
		p.workerQueue <- nil
		workerCount--
	}
	wg.Wait()
}

func worker(task func(), workerQueue chan func(), wg *sync.WaitGroup) {
	for task != nil {
		task()
		task = <-workerQueue
	}
	wg.Done()
}

func (p *WorkerPool) stop(wait bool) {
	p.stopOnce.Do(func() {
		close(p.stopSignal)
		p.stopLock.Lock()
		p.stopped = true
		p.stopLock.Unlock()
		p.wait = wait
		close(p.taskQueue)
	})
	<-p.stoppedChan
}

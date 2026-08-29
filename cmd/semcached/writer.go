package main

import (
	"context"
	"sync"
	"time"
)

// Запись в кэш идёт вне обработчика запроса, потому что она стоит одного
// обращения к эмбеддеру, и клиент не должен за это ждать. Отсюда очередь с
// фиксированным числом рабочих: горутина на промах — это неограниченный
// параллелизм обращений к API эмбеддингов ровно в тот момент, когда провайдер
// и так медленный.
type writeQueue struct {
	jobs    chan func(context.Context)
	wg      sync.WaitGroup
	timeout time.Duration
	onDrop  func()
}

func newWriteQueue(workers, depth int, timeout time.Duration, onDrop func()) *writeQueue {
	q := &writeQueue{
		jobs:    make(chan func(context.Context), depth),
		timeout: timeout,
		onDrop:  onDrop,
	}
	for range workers {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for job := range q.jobs {
				ctx, cancel := context.WithTimeout(context.Background(), q.timeout)
				job(ctx)
				cancel()
			}
		}()
	}
	return q
}

// enqueue не блокирует обработчик: если очередь полна, запись теряется.
// Потерянная запись — это будущий промах кэша, а заблокированный обработчик —
// это отказ в обслуживании.
func (q *writeQueue) enqueue(job func(context.Context)) {
	select {
	case q.jobs <- job:
	default:
		if q.onDrop != nil {
			q.onDrop()
		}
	}
}

// close дожидается уже начатых записей, чтобы ответ, за который заплатили,
// не потерялся при перезапуске.
func (q *writeQueue) close() {
	close(q.jobs)
	q.wg.Wait()
}

// drain возвращается, когда очередь разобрана. Верно при одном рабочем:
// используется тестами, чтобы не спать в ожидании записи.
func (q *writeQueue) drain() {
	done := make(chan struct{})
	q.jobs <- func(context.Context) { close(done) }
	<-done
}

// enqueue у Server: без очереди запись идёт синхронно. Так ведёт себя нулевое
// значение — потерять запись молча хуже, чем подождать.
func (s *Server) enqueue(job func(context.Context)) {
	if s.writes == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		job(ctx)
		return
	}
	s.writes.enqueue(job)
}

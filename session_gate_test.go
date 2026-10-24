package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type sessionGateAttempt struct {
	done    chan struct{}
	release func()
	err     error
}

// 测试结束时取消并回收所有等待者，断言失败也不会留下持锁协程。
func startSessionGateAttempt(t *testing.T, gate *accountSessionGate, ctx context.Context, write bool) *sessionGateAttempt {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	attempt := &sessionGateAttempt{done: make(chan struct{})}
	go func() {
		attempt.release, attempt.err = gate.acquire(ctx, write)
		close(attempt.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-attempt.done:
			if attempt.release != nil {
				attempt.release()
			}
		case <-time.After(2 * time.Second):
			t.Error("会话门等待者取消后没有退出")
		}
	})
	return attempt
}

func awaitSessionGateAttempt(t *testing.T, attempt *sessionGateAttempt) {
	t.Helper()
	select {
	case <-attempt.done:
	case <-time.After(2 * time.Second):
		t.Fatal("会话门没有按预期放行或取消")
	}
}

func requireSessionGateSuccess(t *testing.T, attempt *sessionGateAttempt) func() {
	t.Helper()
	awaitSessionGateAttempt(t, attempt)
	if attempt.err != nil || attempt.release == nil {
		t.Fatalf("会话门获取失败：%v", attempt.err)
	}
	return attempt.release
}

func requireSessionGateWaiting(t *testing.T, attempt *sessionGateAttempt) {
	t.Helper()
	select {
	case <-attempt.done:
		t.Fatal("会话门提前放行了应等待的请求")
	default:
	}
}

// 根据锁内状态确认写方已经排队，不依赖固定睡眠猜测协程调度顺序。
func waitSessionGateWriters(t *testing.T, gate *accountSessionGate, count int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		gate.mu.Lock()
		actual := len(gate.writers)
		gate.mu.Unlock()
		if actual == count {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("写等待人数不符：实际 %d，期望 %d", actual, count)
		case <-ticker.C:
		}
	}
}

func TestSessionGateAllowsParallelReadersAndIdempotentRelease(t *testing.T) {
	var gate accountSessionGate
	first := startSessionGateAttempt(t, &gate, context.Background(), false)
	releaseFirst := requireSessionGateSuccess(t, first)
	second := startSessionGateAttempt(t, &gate, context.Background(), false)
	releaseSecond := requireSessionGateSuccess(t, second)
	gate.mu.Lock()
	readers := gate.readers
	gate.mu.Unlock()
	if readers != 2 {
		t.Fatalf("正常生图未保持并行读：%d", readers)
	}
	var releases sync.WaitGroup
	for range 8 {
		releases.Go(releaseFirst)
	}
	releases.Wait()
	writer := startSessionGateAttempt(t, &gate, context.Background(), true)
	waitSessionGateWriters(t, &gate, 1)
	requireSessionGateWaiting(t, writer)
	releaseSecond()
	releaseWriter := requireSessionGateSuccess(t, writer)
	releaseWriter()
	releaseWriter()
	requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
}

func TestSessionGateWriterWaitsForEveryActiveReader(t *testing.T) {
	var gate accountSessionGate
	releaseFirst := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
	releaseSecond := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
	writer := startSessionGateAttempt(t, &gate, context.Background(), true)
	waitSessionGateWriters(t, &gate, 1)
	requireSessionGateWaiting(t, writer)
	releaseFirst()
	requireSessionGateWaiting(t, writer)
	releaseSecond()
	requireSessionGateSuccess(t, writer)
}

func TestSessionGateWritersTakePriorityInArrivalOrder(t *testing.T) {
	var gate accountSessionGate
	releaseInitial := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
	firstWriter := startSessionGateAttempt(t, &gate, context.Background(), true)
	waitSessionGateWriters(t, &gate, 1)
	secondWriter := startSessionGateAttempt(t, &gate, context.Background(), true)
	waitSessionGateWriters(t, &gate, 2)
	lateReader := startSessionGateAttempt(t, &gate, context.Background(), false)
	requireSessionGateWaiting(t, lateReader)
	releaseInitial()
	releaseFirst := requireSessionGateSuccess(t, firstWriter)
	requireSessionGateWaiting(t, secondWriter)
	requireSessionGateWaiting(t, lateReader)
	releaseFirst()
	releaseSecond := requireSessionGateSuccess(t, secondWriter)
	requireSessionGateWaiting(t, lateReader)
	releaseSecond()
	requireSessionGateSuccess(t, lateReader)
}

func TestSessionGateCancelledWriterUnblocksReaders(t *testing.T) {
	var gate accountSessionGate
	requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := startSessionGateAttempt(t, &gate, ctx, true)
	waitSessionGateWriters(t, &gate, 1)
	reader := startSessionGateAttempt(t, &gate, context.Background(), false)
	cancel()
	awaitSessionGateAttempt(t, writer)
	if !errors.Is(writer.err, context.Canceled) || writer.release != nil {
		t.Fatal("已取消的写方仍持有会话门")
	}
	// 初始读方仍在使用，取消写等待后新读方也应立即加入，而不是等到全部读方退出。
	requireSessionGateSuccess(t, reader)
	waitSessionGateWriters(t, &gate, 0)
}

func TestSessionGateCancelledHeadWriterPreservesNextWriter(t *testing.T) {
	var gate accountSessionGate
	releaseInitial := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstWriter := startSessionGateAttempt(t, &gate, ctx, true)
	waitSessionGateWriters(t, &gate, 1)
	secondWriter := startSessionGateAttempt(t, &gate, context.Background(), true)
	waitSessionGateWriters(t, &gate, 2)
	cancel()
	awaitSessionGateAttempt(t, firstWriter)
	if !errors.Is(firstWriter.err, context.Canceled) {
		t.Fatal("队首写方未按取消退出")
	}
	waitSessionGateWriters(t, &gate, 1)
	releaseInitial()
	requireSessionGateSuccess(t, secondWriter)
}

func TestSessionGateWaitingReaderCancellationDoesNotLeak(t *testing.T) {
	var gate accountSessionGate
	releaseWriter := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), true))
	ctx, cancel := context.WithCancel(context.Background())
	reader := startSessionGateAttempt(t, &gate, ctx, false)
	cancel()
	awaitSessionGateAttempt(t, reader)
	if !errors.Is(reader.err, context.Canceled) || reader.release != nil {
		t.Fatal("已取消的读方仍持有会话门")
	}
	releaseWriter()
	requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), true))
}

func TestSessionGateCancelledContextNeverAcquiresAvailableGate(t *testing.T) {
	for _, write := range []bool{false, true} {
		var gate accountSessionGate
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		release, err := gate.acquire(ctx, write)
		if !errors.Is(err, context.Canceled) || release != nil {
			t.Fatal("已经取消的上下文仍获得空闲会话门")
		}
		requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), true))
	}
}

func TestSessionGateWaitingDeadlineDoesNotBlockLaterRequests(t *testing.T) {
	for _, write := range []bool{false, true} {
		var gate accountSessionGate
		releaseInitial := requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), true))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		attempt := startSessionGateAttempt(t, &gate, ctx, write)
		awaitSessionGateAttempt(t, attempt)
		cancel()
		if !errors.Is(attempt.err, context.DeadlineExceeded) || attempt.release != nil {
			t.Fatal("等待超时没有原样返回上下文错误，或仍持有会话门")
		}
		releaseInitial()
		requireSessionGateSuccess(t, startSessionGateAttempt(t, &gate, context.Background(), true))
	}
}

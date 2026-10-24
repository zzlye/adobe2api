package main

import (
	"context"
	"sync"
)

// accountSessionGate 允许同账号并行使用会话，续期时等待已有使用者退出并阻止新使用者插队。
// 写入者按到达顺序取得独占权，零值即可使用；等待过程支持取消，不需要额外的等待协程。
type accountSessionGate struct {
	mu      sync.Mutex
	readers int
	writing bool
	writers []chan struct{}
	changed chan struct{}
}

func (g *accountSessionGate) acquire(ctx context.Context, write bool) (release func(), err error) {
	g.mu.Lock()
	var ticket chan struct{}
	if write {
		// 独立票据保证写等待的先后顺序，唤醒后不能由后来的续期抢占。
		ticket = make(chan struct{})
		g.writers = append(g.writers, ticket)
	}
	for {
		if err := ctx.Err(); err != nil {
			if write {
				for index, queued := range g.writers {
					if queued == ticket {
						copy(g.writers[index:], g.writers[index+1:])
						g.writers[len(g.writers)-1] = nil
						g.writers = g.writers[:len(g.writers)-1]
						break
					}
				}
				// 最后一位写等待者取消后，原本被挡住的读方应立即恢复。
				g.notifyLocked()
			}
			g.mu.Unlock()
			return nil, err
		}
		allowed := !g.writing && len(g.writers) == 0
		if write {
			allowed = !g.writing && g.readers == 0 && g.writers[0] == ticket
		}
		if allowed {
			if write {
				g.writing = true
				g.writers[0] = nil
				g.writers = g.writers[1:]
			} else {
				g.readers++
			}
			g.mu.Unlock()
			var once sync.Once
			return func() {
				// 取消与正常收尾可以共用释放函数，重复调用不能破坏计数。
				once.Do(func() {
					g.mu.Lock()
					if write {
						g.writing = false
					} else {
						g.readers--
					}
					g.notifyLocked()
					g.mu.Unlock()
				})
			}, nil
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
	}
}

// 调用方持有状态锁时广播变化，等待者会重新检查条件，避免遗漏释放或取消通知。
func (g *accountSessionGate) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

package userstats

import "sync/atomic"

// saturatingUint64 是 README §4.1 要求的饱和 u64 计数器：溢出时钉在 MaxUint64 并置位
// health.counter_overflow，禁止回绕。回绕会让采集端把一次溢出读成一次巨额负增量，
// 按 §5.1 的单调性约束触发失败关闭，却无法区分「真的溢出」与「实现写错」。
type saturatingUint64 struct {
	value atomic.Uint64
}

const maxUint64 = ^uint64(0)

// add 累加 delta，返回本次是否发生截断。
func (c *saturatingUint64) add(delta uint64) (truncated bool) {
	if delta == 0 {
		return false
	}
	for {
		current := c.value.Load()
		remaining := maxUint64 - current
		if delta > remaining {
			if c.value.CompareAndSwap(current, maxUint64) {
				return true
			}
			continue
		}
		if c.value.CompareAndSwap(current, current+delta) {
			return false
		}
	}
}

func (c *saturatingUint64) load() uint64 {
	return c.value.Load()
}

// SPDX-License-Identifier: Apache-2.0

// 分钟聚合（根 README §6）：秒级样本在内存按服务端接收时间累积到分钟桶，
// 分钟结束后写入 metrics_1m。「不把最后一帧当平均」——窗口内样本均值；
// unknown 质量组不纳入对应字段的平均（该字段记 NULL）。
package store

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// aggSweepInterval 为过期分钟桶的清扫周期。
const aggSweepInterval = 5 * time.Second

// fAcc 为单字段累加器：sum/n，n=0 表示本分钟内无有效样本（写库为 NULL）。
type fAcc struct {
	sum float64
	n   int64
}

func (a *fAcc) add(v float64) { a.sum += v; a.n++ }

func (a fAcc) avg() sql.NullFloat64 {
	if a.n == 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: a.sum / float64(a.n), Valid: true}
}

// metricBucket 为一个节点当前分钟的累积桶。
type metricBucket struct {
	minute  int64 // 分钟起点 Unix 秒
	samples int64
	firstAt time.Time
	lastAt  time.Time

	cpu          fAcc
	cpuMax       float64
	cpuMaxOK     bool
	load1        fAcc
	load5        fAcc
	load15       fAcc
	mem          fAcc
	swap         fAcc
	disk         fAcc
	netRX, netTX fAcc
	tcp          fAcc
	udp          fAcc
	procs        fAcc
}

// Aggregator 为分钟聚合器。flush 回调须非阻塞（写队列入队）。
type Aggregator struct {
	flush func(MetricRow)

	mu      sync.Mutex
	buckets map[string]*metricBucket

	stop chan struct{}
	done chan struct{}
}

// NewAggregator 创建聚合器并启动清扫 goroutine（把已过期的分钟桶冲刷出去，
// 覆盖节点停止上报的情况）。Stop 后不再自动冲刷。
func NewAggregator(flush func(MetricRow)) *Aggregator {
	a := &Aggregator{
		flush:   flush,
		buckets: make(map[string]*metricBucket),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go a.sweepLoop()
	return a
}

// Add 加入一个样本。recv 为服务端接收时间（分钟归属按它计算）。
func (a *Aggregator) Add(nodeID string, m *metrics.Metrics, recv time.Time) {
	minute := recv.Unix() / 60 * 60
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.buckets[nodeID]
	if b == nil || b.minute != minute {
		if b != nil {
			a.flushLocked(nodeID, b)
		}
		b = &metricBucket{minute: minute, firstAt: recv}
		a.buckets[nodeID] = b
	}
	b.samples++
	b.lastAt = recv
	q := m.Quality
	if groupKnown(q, "cpu") {
		b.cpu.add(m.CPU)
		if !b.cpuMaxOK || m.CPU > b.cpuMax {
			b.cpuMax = m.CPU
		}
		b.cpuMaxOK = true
	}
	if groupKnown(q, "sys") {
		b.load1.add(m.Load[0])
		b.load5.add(m.Load[1])
		b.load15.add(m.Load[2])
		b.tcp.add(float64(m.TCP))
		b.udp.add(float64(m.UDP))
		b.procs.add(float64(m.Procs))
	}
	if groupKnown(q, "mem") {
		b.mem.add(float64(m.MemUsed))
	}
	if groupKnown(q, "swap") {
		b.swap.add(float64(m.SwapUsed))
	}
	if groupKnown(q, "disk") {
		b.disk.add(float64(m.DiskUsed))
	}
	if groupKnown(q, "net_rate") {
		b.netRX.add(m.NetRX)
		b.netTX.add(m.NetTX)
	}
}

// FlushAll 冲刷全部桶（含当前未完成分钟；重启重叠写由 SQL 加权合并兜底）。
func (a *Aggregator) FlushAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, b := range a.buckets {
		a.flushLocked(id, b)
		delete(a.buckets, id)
	}
}

// Stop 停止清扫 goroutine；之后可再 FlushAll 做最终冲刷。
func (a *Aggregator) Stop() {
	close(a.stop)
	<-a.done
}

func (a *Aggregator) sweepLoop() {
	defer close(a.done)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("store: 聚合清扫 panic（已降级）：%v", r)
		}
	}()
	ticker := time.NewTicker(aggSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case now := <-ticker.C:
			a.sweep(now)
		}
	}
}

// sweep 冲刷所有已结束分钟的桶。
func (a *Aggregator) sweep(now time.Time) {
	current := now.Unix() / 60 * 60
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, b := range a.buckets {
		if b.minute < current {
			a.flushLocked(id, b)
			delete(a.buckets, id)
		}
	}
}

// flushLocked 把桶转换为 MetricRow 并交给 flush 回调。调用方须持有 a.mu。
func (a *Aggregator) flushLocked(nodeID string, b *metricBucket) {
	row := MetricRow{
		NodeID:  nodeID,
		TsMin:   b.minute,
		Samples: b.samples,
		CoverMS: b.lastAt.Sub(b.firstAt).Milliseconds(),

		CPUAvg:      b.cpu.avg(),
		Load1Avg:    b.load1.avg(),
		Load5Avg:    b.load5.avg(),
		Load15Avg:   b.load15.avg(),
		MemUsedAvg:  b.mem.avg(),
		SwapUsedAvg: b.swap.avg(),
		DiskUsedAvg: b.disk.avg(),
		NetRXAvg:    b.netRX.avg(),
		NetTXAvg:    b.netTX.avg(),
		TCPAvg:      b.tcp.avg(),
		UDPAvg:      b.udp.avg(),
		ProcsAvg:    b.procs.avg(),
	}
	if b.cpuMaxOK {
		row.CPUMax = sql.NullFloat64{Float64: b.cpuMax, Valid: true}
	}
	a.flush(row)
}

// groupKnown 判断某数据组质量是否有效（nil Quality 全部有效）。
// 与 api 包的 qualityKnown 语义一致，独立实现以避免包间依赖。
func groupKnown(q *metrics.Quality, group string) bool {
	if q == nil {
		return true
	}
	var v string
	switch group {
	case "cpu":
		v = q.CPU
	case "mem":
		v = q.Mem
	case "swap":
		v = q.Swap
	case "disk":
		v = q.Disk
	case "net_rate":
		v = q.NetRate
	case "net_total":
		v = q.NetTotal
	case "sys":
		v = q.Sys
	}
	return v != metrics.QualityUnknown
}

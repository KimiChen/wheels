package userstats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
)

// 载体标记地址不是访问目标，真实目标在子流上（README §4.8 纪律 8）。
var auditSentinelHosts = map[string]struct{}{
	"sp.mux.sing-box.arpa":      {},
	"sp.v2.udp-over-tcp.arpa":   {},
	"sp.udp-over-tcp.arpa":      {},
	"sp.packet-addr.v2fly.arpa": {},
}

const auditMaxHostBytes = 255

// auditRecord 是一条待落盘的访问记录。
type auditRecord struct {
	user       string
	inboundTag string
	network    string
	host       string
	hostSource string
	port       uint16
	up         int64
	down       int64
	durationMs int64
}

// auditWriter 是 §4.8 的同进程 JSONL 旁路。
//
// 定位是运营辅助而非计费依据：不进快照、不走 UDS、不做 fsync，崩溃会丢缓冲区内未落盘的记录，
// 且该丢失不产生 gap 行。启动 fail-hard，运行 fail-open（纪律 12）。
type auditWriter struct {
	nodeID    string
	runtimeID string
	options   AccessLogOptions
	logger    log.ContextLogger

	records chan auditRecord

	seq          atomic.Uint64
	droppedQueue atomic.Uint64
	droppedWrite atomic.Uint64
	lastGapAfter atomic.Uint64

	file   *os.File
	writer *bufio.Writer
	size   int64

	done     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
	closed   atomic.Bool
}

// newAuditWriter 打开审计文件并做启动期硬校验。
//
// 以最终 euid 实际 OpenFile(0600) 一次并 Chmod + Stat 复核属主与权限，任一不符即启动失败
// （纪律 12 的前半，与 §4.6 同档）。
func newAuditWriter(nodeID string, runtimeID string, options AccessLogOptions, logger log.ContextLogger) (*auditWriter, error) {
	if err := checkParentDirectory(options.Path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(options.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, E.Cause(err, "打开访问审计文件 ", options.Path)
	}
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return nil, E.Cause(err, "设置访问审计文件权限")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, E.Cause(err, "读取访问审计文件属性")
	}
	if info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, E.New("访问审计文件权限不是 0600：", info.Mode().Perm().String())
	}
	if err = checkFileOwner(info); err != nil {
		file.Close()
		return nil, err
	}
	// 启动先补残行：崩溃留下的半行若不补 LF，会与重启后第一条粘连，两条记录被下游一起丢弃
	// （纪律 5）。
	if err = repairTrailingNewline(file, info.Size()); err != nil {
		file.Close()
		return nil, err
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, E.Cause(err, "复核访问审计文件大小")
	}
	writer := &auditWriter{
		nodeID:    nodeID,
		runtimeID: runtimeID,
		options:   options,
		logger:    logger,
		records:   make(chan auditRecord, options.QueueSize),
		file:      file,
		writer:    bufio.NewWriterSize(file, 64*1024),
		size:      info.Size(),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go writer.loop()
	return writer, nil
}

// submit 只向有界 channel 投递，由单个 writer goroutine 序列化落盘。
//
// 成功判据是 AND 不是 OR（纪律 2）：TCP 要求 up > 0 && down > 0，UDP 只要求 up > 0。
// up + down > 0 会把端口扫描与被 RST 的 ClientHello 逐条渲染成「成功访问」。
func (w *auditWriter) submit(record auditRecord) {
	defer func() {
		// 运行期 fail-open：审计的任何 panic 都不得阻断转发（纪律 12）。
		if recovered := recover(); recovered != nil && w.logger != nil {
			w.logger.Error("访问审计投递 panic：", recovered)
		}
	}()
	if record.network == "tcp" {
		if record.up <= 0 || record.down <= 0 {
			return
		}
	} else if record.up <= 0 {
		return
	}
	if _, sentinel := auditSentinelHosts[record.host]; sentinel {
		return
	}
	// SIGHUP 会重建 services[] 实例：旧 writer 关闭后仍可能收到在途连接的投递，
	// 此时静默丢弃而不是往无人消费的 channel 里堆。
	if w.closed.Load() {
		return
	}
	select {
	case w.records <- record:
	default:
		w.droppedQueue.Add(1)
	}
}

func (w *auditWriter) loop() {
	defer close(w.stopped)
	defer func() {
		if recovered := recover(); recovered != nil && w.logger != nil {
			w.logger.Error("访问审计 writer panic：", recovered)
		}
	}()
	interval := time.Duration(w.options.FlushIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case record := <-w.records:
			w.write(w.encode(record))
		case <-ticker.C:
			w.flushGap()
			w.flush()
			w.checkRotation()
		case <-w.done:
			return
		}
	}
}

// Close 的关闭序：有界等待 → 排空 channel → Flush + Sync → 写一条 stop 行。
// 数据 channel 永不 close，否则在途的 Close() 投递会 panic 并打掉进程（纪律 13）。
func (w *auditWriter) Close() error {
	w.stopOnce.Do(func() {
		w.closed.Store(true)
		close(w.done)
		<-w.stopped
		deadline := time.After(2 * time.Second)
	drain:
		for {
			select {
			case record := <-w.records:
				w.write(w.encode(record))
			case <-deadline:
				break drain
			default:
				break drain
			}
		}
		w.flushGap()
		w.write(w.encodeEvent("stop", 0, nil, ""))
		w.flush()
		_ = w.file.Sync()
		_ = w.file.Close()
	})
	return nil
}

func (w *auditWriter) encode(record auditRecord) []byte {
	payload := map[string]any{
		"seq":      w.seq.Add(1),
		"ts":       time.Now().UnixMilli(),
		"node":     w.nodeID,
		"run":      w.runtimeID,
		"user":     record.user,
		"in":       record.inboundTag,
		"net":      record.network,
		"host":     normalizeAuditHost(record.host),
		"host_src": record.hostSource,
		"port":     record.port,
		"up":       record.up,
		"down":     record.down,
		"ms":       record.durationMs,
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return append(line, '\n')
}

// encodeEvent 编码 gap 与 stop 两类事件行。
//
// 边界不确定时 n 写 null，不得合成一个不存在的区间。
func (w *auditWriter) encodeEvent(event string, after uint64, count *uint64, reason string) []byte {
	payload := map[string]any{
		"seq": w.seq.Add(1),
		"ev":  event,
	}
	if event == "gap" {
		payload["after"] = after
		if count == nil {
			payload["n"] = nil
		} else {
			payload["n"] = *count
		}
		payload["reason"] = reason
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return append(line, '\n')
}

func (w *auditWriter) flushGap() {
	dropped := w.droppedQueue.Swap(0)
	if dropped == 0 {
		return
	}
	after := w.seq.Load()
	w.write(w.encodeEvent("gap", after, &dropped, "queue_full"))
	w.lastGapAfter.Store(after)
}

// write 落盘一行。
//
// 一条记录一次写出：bufio 在缓冲将满时按字节边界拆分，不是记录边界，这是分片写的常态而非
// 罕见竞态（纪律 4）。
func (w *auditWriter) write(line []byte) {
	if len(line) == 0 {
		return
	}
	if len(line) > w.writer.Available() {
		w.flush()
	}
	n, err := w.writer.Write(line)
	w.size += int64(n)
	if err != nil {
		w.resetWriter()
		return
	}
	if w.size >= w.options.MaxBytes {
		w.flush()
		w.rotate()
	}
}

// flush 提交缓冲。
//
// bufio.Writer 一旦 Flush 出错即置粘滞 b.err，此后 Write 零系统调用返回、channel 永不满、
// 丢弃计数结构性恒为 0——磁盘腾空也不自愈。失败路径必须 Reset 并累加 dropped_write（纪律 3）。
func (w *auditWriter) flush() {
	if err := w.writer.Flush(); err != nil {
		w.resetWriter()
	}
}

func (w *auditWriter) resetWriter() {
	w.droppedWrite.Add(1)
	w.writer.Reset(w.file)
	if w.logger != nil {
		w.logger.Warn("访问审计写入失败，已复位 writer；累计丢弃 ", w.droppedWrite.Load(), " 次")
	}
}

// checkRotation 用 os.SameFile 比对当前 fd 与路径上的 inode，不同则重开。
//
// 否则轮转或外部删除之后，写入会落进已删除 inode 的黑洞（纪律 9）。
func (w *auditWriter) checkRotation() {
	pathInfo, err := os.Stat(w.options.Path)
	if err == nil {
		fileInfo, statErr := w.file.Stat()
		if statErr == nil && os.SameFile(pathInfo, fileInfo) {
			return
		}
	}
	w.reopen()
}

func (w *auditWriter) reopen() {
	w.flush()
	_ = w.file.Close()
	file, err := os.OpenFile(w.options.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		w.droppedWrite.Add(1)
		if w.logger != nil {
			w.logger.Error("访问审计文件重开失败：", err)
		}
		return
	}
	w.file = file
	w.writer.Reset(file)
	if info, statErr := file.Stat(); statErr == nil {
		w.size = info.Size()
	} else {
		w.size = 0
	}
}

func (w *auditWriter) rotate() {
	rotated := w.options.Path + "." + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(w.options.Path, rotated); err != nil {
		w.droppedWrite.Add(1)
		return
	}
	w.reopen()
	w.enforceTotalCap()
}

// enforceTotalCap 超过 max_total_bytes 时删最老的已轮转文件并写一条 gap；
// 无对象可删时只累加计数、不写 gap——否则自指 gap 会把真事件放大（纪律 10）。
func (w *auditWriter) enforceTotalCap() {
	if w.options.MaxTotalBytes <= 0 {
		return
	}
	for {
		total, oldest, err := auditTotalSize(w.options.Path)
		if err != nil || total <= w.options.MaxTotalBytes {
			return
		}
		if oldest == "" {
			w.droppedWrite.Add(1)
			return
		}
		if err = os.Remove(oldest); err != nil {
			w.droppedWrite.Add(1)
			return
		}
		after := w.seq.Load()
		w.write(w.encodeEvent("gap", after, nil, "total_cap"))
	}
}

func auditTotalSize(path string) (total int64, oldest string, err error) {
	directory := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, "", err
	}
	var rotated []string
	for _, entry := range entries {
		name := entry.Name()
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		total += info.Size()
		if name != base {
			rotated = append(rotated, filepath.Join(directory, name))
		}
	}
	sort.Strings(rotated)
	if len(rotated) > 0 {
		oldest = rotated[0]
	}
	return total, oldest, nil
}

// normalizeAuditHost 归一化到此为止：只做小写与去掉一个尾点，不引入 idna.Lookup（纪律 7）；
// 并按纪律 6 截断到 255 字节且转为合法 UTF-8——sniff 出的 SNI 无上限，是攻击者可控字节串。
func normalizeAuditHost(host string) string {
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	host = strings.ToValidUTF8(host, "�")
	if len(host) > auditMaxHostBytes {
		host = host[:auditMaxHostBytes]
		for len(host) > 0 && !utf8.ValidString(host) {
			host = host[:len(host)-1]
		}
	}
	return host
}

// auditHost 按 metadata.Domain → Destination.Fqdn → Destination.Addr 三级降级取值。
func auditHost(metadata adapter.InboundContext) (host string, source string) {
	if metadata.Domain != "" {
		return metadata.Domain, "sniff"
	}
	if metadata.Destination.Fqdn != "" {
		return metadata.Destination.Fqdn, "fqdn"
	}
	if metadata.Destination.Addr.IsValid() {
		return metadata.Destination.Addr.String(), "ip"
	}
	return "", "ip"
}

func repairTrailingNewline(file *os.File, size int64) error {
	if size == 0 {
		return nil
	}
	var tail [1]byte
	if _, err := file.ReadAt(tail[:], size-1); err != nil {
		// 以 O_WRONLY 打开时无法回读，退化为保守补一个 LF：多一个空行下游会跳过，
		// 半行粘连则会连累两条记录。
		if _, writeErr := file.Write([]byte("\n")); writeErr != nil {
			return E.Cause(writeErr, "补齐访问审计残行")
		}
		return nil
	}
	if tail[0] == '\n' {
		return nil
	}
	if _, err := file.Write([]byte("\n")); err != nil {
		return E.Cause(err, "补齐访问审计残行")
	}
	return nil
}

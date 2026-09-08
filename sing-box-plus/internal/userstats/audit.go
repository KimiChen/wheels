package userstats

import (
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

const (
	auditMaxHostBytes  = 255
	auditFileBufferMax = 8 * 1024
)

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

// auditWriter 是 §4.8 的同进程 JSONL 旁路，**逐计费身份一个文件**。
//
// 逐用户分文件的理由是「数据主体分离」——按人删除、按人交付、按人设保留期——
// 而**不是**抗篡改：文件仍与数据面同 uid，被攻破的数据面可以改写其中任意一个，
// 分成多少个文件都一样。抗篡改属于 D7 的重新评估范围，见 §11.2。
//
// 定位仍是运营辅助而非计费依据：不进快照、不走 UDS、不做 fsync，崩溃会丢缓冲区内未落盘的
// 记录，且该丢失不产生 gap 行。启动 fail-hard，运行 fail-open（纪律 12）。
type auditWriter struct {
	nodeID    string
	runtimeID string
	options   AccessLogOptions
	logger    log.ContextLogger

	records chan auditRecord

	droppedWrite atomic.Uint64

	// 队列满时我们**知道**是谁的记录被丢了，因此丢弃数按身份计，
	// gap 行落进那个人的文件而不是笼统记在一处。
	dropMutex sync.Mutex
	dropped   map[string]uint64

	// files / order 只由 writer goroutine 访问，不需要锁。
	files map[string]*auditFile
	order []string // LRU：最近使用排在末尾

	// inspect 让测试能在 writer goroutine 上同步观察上面这些状态。
	// 没有它的话，用例只能从外部直接读 files，那本身就是一次数据竞争；
	// 而为了让测试能观察就给写入热路径加一把锁，是让被测代码迁就测试。
	inspect chan func()

	done     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
	closed   atomic.Bool
}

// newAuditWriter 校验目录并启动 writer。
//
// identities 是配置里已知的全部计费身份，只用于启动期的文件名碰撞检查；
// 文件本身按需惰性创建——从未产生过成功访问的身份不该凭空多出一个空文件。
func newAuditWriter(nodeID string, runtimeID string, options AccessLogOptions, identities []string, logger log.ContextLogger) (*auditWriter, error) {
	if err := checkAuditDirectory(options.Directory); err != nil {
		return nil, err
	}
	if err := checkAuditNameCollisions(identities); err != nil {
		return nil, err
	}
	writer := &auditWriter{
		nodeID:    nodeID,
		runtimeID: runtimeID,
		options:   options,
		logger:    logger,
		records:   make(chan auditRecord, options.QueueSize),
		dropped:   make(map[string]uint64),
		files:     make(map[string]*auditFile),
		inspect:   make(chan func()),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go writer.loop()
	return writer, nil
}

// checkAuditDirectory 要求目录已存在、是目录、且不是符号链接。
//
// 不自动创建：审计目录的属主与权限是部署决策，由 tmpfiles/StateDirectory 负责，
// 让进程顺手 mkdir 会掩盖「部署漏配了」这件事。
func checkAuditDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return E.Cause(err, "检查访问审计目录 ", directory)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return E.New("访问审计目录是符号链接，拒绝使用：", directory)
	}
	if !info.IsDir() {
		return E.New("访问审计目录不是目录：", directory)
	}
	return checkFileOwner(info)
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
		w.dropMutex.Lock()
		w.dropped[record.user]++
		w.dropMutex.Unlock()
	}
}

func (w *auditWriter) loop() {
	defer close(w.stopped)
	defer func() {
		if recovered := recover(); recovered != nil && w.logger != nil {
			w.logger.Error("访问审计 writer panic：", recovered)
		}
	}()
	ticker := time.NewTicker(time.Duration(w.options.FlushIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case record := <-w.records:
			w.writeRecord(record)
		case fn := <-w.inspect:
			fn()
		case <-ticker.C:
			w.flushGaps()
			w.flushAll()
			w.checkRotationAll()
			w.enforceTotalCap()
		case <-w.done:
			return
		}
	}
}

// Close 的关闭序：有界等待 → 排空 channel → 逐文件 Flush + Sync → 每个打开的文件各写一条
// stop 行。数据 channel 永不 close，否则在途的 Close() 投递会 panic 并打掉进程（纪律 13）。
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
				w.writeRecord(record)
			case <-deadline:
				break drain
			default:
				break drain
			}
		}
		w.flushGaps()
		for _, file := range w.files {
			w.write(file, w.encodeEvent(file, "stop", 0, nil, ""))
			w.flush(file)
			_ = file.file.Sync()
			_ = file.file.Close()
		}
		w.files = make(map[string]*auditFile)
		w.order = nil
	})
	return nil
}

// fileFor 取得某身份的文件，必要时打开；打开数超过上限时按 LRU 关掉最久未用的一个。
//
// 上限存在的理由：200 个身份就是 200 个 fd 与 200 份写缓冲，而身份数上限是 max_identities
// （默认 4096）。没有上限时，一个身份多的节点会在 fd 与内存两头同时失控。
func (w *auditWriter) fileFor(identity string) *auditFile {
	if file, exists := w.files[identity]; exists {
		w.touch(identity)
		return file
	}
	if len(w.files) >= w.options.MaxOpenFiles && len(w.order) > 0 {
		evicted := w.order[0]
		w.order = w.order[1:]
		if victim, exists := w.files[evicted]; exists {
			w.flush(victim)
			_ = victim.file.Close()
			delete(w.files, evicted)
		}
	}
	path := filepath.Join(w.options.Directory, auditFileName(identity))
	file, err := openAuditFile(path, identity, auditFileBufferMax)
	if err != nil {
		w.droppedWrite.Add(1)
		if w.logger != nil {
			w.logger.Error("打开访问审计文件失败：", err)
		}
		return nil
	}
	w.files[identity] = file
	w.order = append(w.order, identity)
	return file
}

func (w *auditWriter) touch(identity string) {
	for index, item := range w.order {
		if item == identity {
			w.order = append(append(w.order[:index:index], w.order[index+1:]...), identity)
			return
		}
	}
	w.order = append(w.order, identity)
}

// onWriterGoroutine 在 writer goroutine 上同步执行 fn，供测试观察内部状态。
// writer 已停止时直接返回 false，避免用例在关闭后死等。
func (w *auditWriter) onWriterGoroutine(fn func()) bool {
	done := make(chan struct{})
	select {
	case w.inspect <- func() { fn(); close(done) }:
		<-done
		return true
	case <-w.stopped:
		return false
	}
}

func (w *auditWriter) writeRecord(record auditRecord) {
	file := w.fileFor(record.user)
	if file == nil {
		return
	}
	w.write(file, w.encode(file, record))
}

func (w *auditWriter) encode(file *auditFile, record auditRecord) []byte {
	file.seq++
	payload := map[string]any{
		"seq":      file.seq,
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
func (w *auditWriter) encodeEvent(file *auditFile, event string, after uint64, count *uint64, reason string) []byte {
	file.seq++
	payload := map[string]any{
		"seq": file.seq,
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

// flushGaps 把每个身份的队列丢弃数写成该身份文件里的一条 gap 行。
func (w *auditWriter) flushGaps() {
	w.dropMutex.Lock()
	pending := w.dropped
	if len(pending) == 0 {
		w.dropMutex.Unlock()
		return
	}
	w.dropped = make(map[string]uint64)
	w.dropMutex.Unlock()

	identities := make([]string, 0, len(pending))
	for identity := range pending {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	for _, identity := range identities {
		file := w.fileFor(identity)
		if file == nil {
			continue
		}
		count := pending[identity]
		w.write(file, w.encodeEvent(file, "gap", file.seq, &count, "queue_full"))
	}
}

// write 落盘一行。
//
// 一条记录一次写出：bufio 在缓冲将满时按字节边界拆分，不是记录边界，这是分片写的常态而非
// 罕见竞态（纪律 4）。
func (w *auditWriter) write(file *auditFile, line []byte) {
	if len(line) == 0 {
		return
	}
	if len(line) > file.writer.Available() {
		w.flush(file)
	}
	n, err := file.writer.Write(line)
	file.size += int64(n)
	if err != nil {
		w.resetWriter(file)
		return
	}
	if file.size >= w.options.MaxBytes {
		w.flush(file)
		w.rotate(file)
	}
}

// flush 提交缓冲。
//
// bufio.Writer 一旦 Flush 出错即置粘滞 b.err，此后 Write 零系统调用返回、channel 永不满、
// 丢弃计数结构性恒为 0——磁盘腾空也不自愈。失败路径必须 Reset 并累加 dropped_write（纪律 3）。
func (w *auditWriter) flush(file *auditFile) {
	if err := file.writer.Flush(); err != nil {
		w.resetWriter(file)
	}
}

func (w *auditWriter) flushAll() {
	for _, file := range w.files {
		w.flush(file)
	}
}

func (w *auditWriter) resetWriter(file *auditFile) {
	w.droppedWrite.Add(1)
	file.writer.Reset(file.file)
	if w.logger != nil {
		w.logger.Warn("访问审计写入失败，已复位 writer：", file.path,
			"；累计丢弃 ", w.droppedWrite.Load(), " 次")
	}
}

// checkRotationAll 用 os.SameFile 比对每个打开文件的 fd 与路径上的 inode，不同则重开。
//
// 否则轮转或外部删除之后，写入会落进已删除 inode 的黑洞（纪律 9）。
func (w *auditWriter) checkRotationAll() {
	for _, file := range w.files {
		pathInfo, err := os.Stat(file.path)
		if err == nil {
			fileInfo, statErr := file.file.Stat()
			if statErr == nil && os.SameFile(pathInfo, fileInfo) {
				continue
			}
		}
		w.reopen(file)
	}
}

func (w *auditWriter) reopen(file *auditFile) {
	w.flush(file)
	_ = file.file.Close()
	opened, err := os.OpenFile(file.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		w.droppedWrite.Add(1)
		if w.logger != nil {
			w.logger.Error("访问审计文件重开失败：", err)
		}
		return
	}
	file.file = opened
	file.writer.Reset(opened)
	if info, statErr := opened.Stat(); statErr == nil {
		file.size = info.Size()
	} else {
		file.size = 0
	}
}

func (w *auditWriter) rotate(file *auditFile) {
	rotated := file.path + "." + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(file.path, rotated); err != nil {
		w.droppedWrite.Add(1)
		return
	}
	w.reopen(file)
}

// enforceTotalCap 是**全局**上限：超过 max_total_bytes 时跨全部身份挑最老的已轮转文件删除，
// 并把 gap 写进它所属身份的文件；无对象可删时只累加计数、不写 gap——
// 否则自指 gap 会把真事件放大（纪律 10）。
//
// 上限之所以是全局而非逐身份：它保护的是磁盘，而磁盘是共享的。
// 逐身份的保留期属于数据主体策略，做法是删掉那个人的文件，不走这条路径。
func (w *auditWriter) enforceTotalCap() {
	if w.options.MaxTotalBytes <= 0 {
		return
	}
	for {
		total, oldest, err := auditDirectorySize(w.options.Directory)
		if err != nil || total <= w.options.MaxTotalBytes {
			return
		}
		if oldest == "" {
			w.droppedWrite.Add(1)
			return
		}
		identity, known := auditIdentityFromFileName(filepath.Base(oldest))
		if err = os.Remove(oldest); err != nil {
			w.droppedWrite.Add(1)
			return
		}
		if !known {
			continue
		}
		if file := w.fileFor(identity); file != nil {
			w.write(file, w.encodeEvent(file, "gap", file.seq, nil, "total_cap"))
		}
	}
}

// auditDirectorySize 统计目录里全部审计文件的总大小，并返回最老的已轮转文件。
func auditDirectorySize(directory string) (total int64, oldest string, err error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, "", err
	}
	var rotated []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, auditFilePrefix) {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		total += info.Size()
		// 已轮转文件形如 access-<b64>.jsonl.<时间戳>，时间戳可直接按字典序比较。
		if strings.Contains(name, auditFileSuffix+".") {
			rotated = append(rotated, filepath.Join(directory, name))
		}
	}
	sort.Slice(rotated, func(i, j int) bool {
		return rotatedTimestamp(rotated[i]) < rotatedTimestamp(rotated[j])
	})
	if len(rotated) > 0 {
		oldest = rotated[0]
	}
	return total, oldest, nil
}

func rotatedTimestamp(path string) string {
	name := filepath.Base(path)
	index := strings.Index(name, auditFileSuffix+".")
	if index < 0 {
		return name
	}
	return name[index+len(auditFileSuffix)+1:]
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
		// 回读失败时退化为保守补一个 LF：多一个空行下游会跳过，
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

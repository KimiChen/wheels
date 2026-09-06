package userstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
)

func newTestAudit(t *testing.T, override func(*AccessLogOptions)) (*auditWriter, string) {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "access.jsonl")
	options := AccessLogOptions{Path: path}
	if err := options.normalize(); err != nil {
		t.Fatalf("规整审计配置失败：%v", err)
	}
	options.FlushIntervalMs = 20
	if override != nil {
		override(&options)
	}
	writer, err := newAuditWriter("node-test", "0123456789abcdef0123456789abcdef", options, log.NewNOPFactory().Logger())
	if err != nil {
		t.Fatalf("创建审计 writer 失败：%v", err)
	}
	t.Cleanup(func() { writer.Close() })
	return writer, path
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败：%v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err = json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("审计行不是合法 JSON：%v（%q）", err, line)
		}
		out = append(out, record)
	}
	return out
}

// TestAuditStartupHardening 覆盖纪律 12 的启动期 fail-hard：权限必须是 0600。
func TestAuditStartupHardening(t *testing.T) {
	_, path := newTestAudit(t, nil)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("审计文件权限必须是 0600，实际 %v", info.Mode().Perm())
	}
	missing := AccessLogOptions{Path: filepath.Join(shortTempDir(t), "nope", "a.jsonl")}
	if err = missing.normalize(); err != nil {
		t.Fatalf("规整失败：%v", err)
	}
	if _, err = newAuditWriter("n", "r", missing, log.NewNOPFactory().Logger()); err == nil {
		t.Fatal("父目录不存在时必须启动失败")
	}
}

// TestAuditSuccessCriteria 是纪律 2：成功判据是 AND 不是 OR。
func TestAuditSuccessCriteria(t *testing.T) {
	writer, path := newTestAudit(t, nil)
	// TCP 要求 up > 0 && down > 0。
	writer.submit(auditRecord{user: "u1", inboundTag: "in", network: "tcp", host: "a.example", hostSource: "sniff", port: 443, up: 10, down: 0})
	writer.submit(auditRecord{user: "u1", inboundTag: "in", network: "tcp", host: "b.example", hostSource: "sniff", port: 443, up: 0, down: 10})
	writer.submit(auditRecord{user: "u1", inboundTag: "in", network: "tcp", host: "c.example", hostSource: "sniff", port: 443, up: 5, down: 7})
	// UDP 只要求 up > 0。
	writer.submit(auditRecord{user: "u1", inboundTag: "in", network: "udp", host: "d.example", hostSource: "fqdn", port: 53, up: 3, down: 0})
	writer.submit(auditRecord{user: "u1", inboundTag: "in", network: "udp", host: "e.example", hostSource: "fqdn", port: 53, up: 0, down: 9})
	writer.Close()

	records := readLines(t, path)
	var hosts []string
	for _, record := range records {
		if record["ev"] != nil {
			continue
		}
		hosts = append(hosts, record["host"].(string))
	}
	if len(hosts) != 2 || hosts[0] != "c.example" || hosts[1] != "d.example" {
		t.Fatalf("成功判据错误，落盘记录为：%v", hosts)
	}
}

// TestAuditSentinelSkipped 是纪律 8：载体标记地址不是访问目标。
func TestAuditSentinelSkipped(t *testing.T) {
	writer, path := newTestAudit(t, nil)
	for host := range auditSentinelHosts {
		writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: host, hostSource: "fqdn", port: 443, up: 1, down: 1})
	}
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "real.example", hostSource: "sniff", port: 443, up: 1, down: 1})
	writer.Close()
	records := readLines(t, path)
	var count int
	for _, record := range records {
		if record["ev"] == nil {
			count++
			if record["host"] != "real.example" {
				t.Fatalf("哨兵地址被写入：%v", record["host"])
			}
		}
	}
	if count != 1 {
		t.Fatalf("应恰好落盘 1 条，实际 %d", count)
	}
}

// TestAuditHostNormalization 是纪律 6、7：截断 255 字节、转合法 UTF-8、只做小写与去一个尾点。
func TestAuditHostNormalization(t *testing.T) {
	if got := normalizeAuditHost("EXAMPLE.COM."); got != "example.com" {
		t.Fatalf("归一化只做小写与去一个尾点，实际 %q", got)
	}
	if got := normalizeAuditHost("example.com.."); got != "example.com." {
		t.Fatalf("只去一个尾点，实际 %q", got)
	}
	long := strings.Repeat("a", 16*1024)
	if got := normalizeAuditHost(long); len(got) != auditMaxHostBytes {
		t.Fatalf("超长 host 必须截到 %d 字节，实际 %d", auditMaxHostBytes, len(got))
	}
	invalid := normalizeAuditHost("a\xffb")
	if strings.ContainsRune(invalid, 0xff) {
		t.Fatalf("非法 UTF-8 必须替换：%q", invalid)
	}

	writer, path := newTestAudit(t, nil)
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: long, hostSource: "sniff", port: 443, up: 1, down: 1})
	writer.Close()
	raw, _ := os.ReadFile(path)
	if len(raw) > 4096 {
		t.Fatalf("单行长度必须有界，实际 %d 字节", len(raw))
	}
}

// TestAuditQueueFullGap 是缺口表达：队列满只累计，随后用一条 gap 行表达，n 为实际丢弃数。
func TestAuditQueueFullGap(t *testing.T) {
	writer, path := newTestAudit(t, func(options *AccessLogOptions) {
		options.QueueSize = 1
		options.FlushIntervalMs = 10_000 // 不让 ticker 抢先消费
	})
	// writer goroutine 可能立刻取走一条，因此多投几条保证溢出。
	for index := 0; index < 64; index++ {
		writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "x.example", hostSource: "sniff", port: 1, up: 1, down: 1})
	}
	if writer.droppedQueue.Load() == 0 {
		t.Fatal("队列满必须累计丢弃数")
	}
	writer.Close()
	records := readLines(t, path)
	var gap map[string]any
	for _, record := range records {
		if record["ev"] == "gap" {
			gap = record
		}
	}
	if gap == nil {
		t.Fatal("丢弃后必须写一条 gap 行")
	}
	if gap["reason"] != "queue_full" {
		t.Fatalf("gap 原因不符：%v", gap["reason"])
	}
	if gap["n"] == nil {
		t.Fatal("队列满的丢弃数是已知的，n 不应为 null")
	}
}

// TestAuditWriteFailureResets 是纪律 3：Flush 出错后必须复位，否则丢弃计数结构性恒为 0。
func TestAuditWriteFailureResets(t *testing.T) {
	writer, _ := newTestAudit(t, nil)
	// 关掉底层 fd，制造粘滞写错误。
	_ = writer.file.Close()
	writer.write([]byte("{\"x\":1}\n"))
	writer.flush()
	if writer.droppedWrite.Load() == 0 {
		t.Fatal("写失败必须累加 dropped_write 并复位 writer")
	}
	// 复位之后 bufio 不应继续携带粘滞错误。
	writer.reopen()
	writer.write([]byte("{\"y\":2}\n"))
	writer.flush()
	before := writer.droppedWrite.Load()
	writer.write([]byte("{\"z\":3}\n"))
	writer.flush()
	if writer.droppedWrite.Load() != before {
		t.Fatal("复位并重开之后不应继续计入写失败")
	}
}

// TestAuditRepairTrailingNewline 是纪律 5：崩溃留下的半行必须在启动时补 LF。
func TestAuditRepairTrailingNewline(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "access.jsonl")
	if err := os.WriteFile(path, []byte("{\"seq\":1}\n{\"seq\":2,\"half\""), 0o600); err != nil {
		t.Fatalf("写入半行失败：%v", err)
	}
	options := AccessLogOptions{Path: path}
	if err := options.normalize(); err != nil {
		t.Fatalf("规整失败：%v", err)
	}
	writer, err := newAuditWriter("n", "r", options, log.NewNOPFactory().Logger())
	if err != nil {
		t.Fatalf("创建 writer 失败：%v", err)
	}
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "x", hostSource: "ip", port: 1, up: 1, down: 1})
	writer.Close()
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines[1] != "{\"seq\":2,\"half\"" {
		t.Fatalf("半行应原样保留并单独成行：%q", lines[1])
	}
	// 补 LF 之后新记录不会与半行粘连：第三行必须能独立解析。
	if !json.Valid([]byte(lines[2])) {
		t.Fatalf("补残行失败，新记录与半行粘连：%q", lines[2])
	}
}

// TestAuditRotationDetection 是纪律 9：外部 mv + rm 之后必须在下一个 flush tick 重开新文件。
func TestAuditRotationDetection(t *testing.T) {
	writer, path := newTestAudit(t, nil)
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "a", hostSource: "ip", port: 1, up: 1, down: 1})
	time.Sleep(60 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除文件失败：%v", err)
	}
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "b", hostSource: "ip", port: 1, up: 1, down: 1})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("文件被外部删除后必须重开，不能继续写进已删除 inode")
	}
	writer.Close()
}

// TestAuditTotalCapNoSelfReferentialGap 是纪律 10：无对象可删时只累加计数、不写 gap。
//
// 判据用 seq 而不是读文件：写一条 gap 必然推进 seq，所以「seq 没动」就等价于「没写 gap」，
// 且不需要为了制造超限而把日志文件本身弄成不可解析的样子。
func TestAuditTotalCapNoSelfReferentialGap(t *testing.T) {
	writer, path := newTestAudit(t, func(options *AccessLogOptions) {
		options.MaxBytes = 64 * 1024
		options.MaxTotalBytes = 64 * 1024
	})
	// 只有主文件、没有任何已轮转文件，因此没有可删对象。
	filler := strings.Repeat("{\"seq\":0}\n", 16*1024)
	if err := os.WriteFile(path, []byte(filler), 0o600); err != nil {
		t.Fatalf("撑大文件失败：%v", err)
	}
	beforeDropped := writer.droppedWrite.Load()
	beforeSeq := writer.seq.Load()
	writer.enforceTotalCap()
	if writer.droppedWrite.Load() == beforeDropped {
		t.Fatal("无对象可删时应累加计数")
	}
	if writer.seq.Load() != beforeSeq {
		t.Fatal("无对象可删时不得写 gap，否则自指 gap 会放大真事件")
	}
	writer.Close()
}

// TestAuditCloseIsIdempotent 是纪律 13：数据 channel 永不 close，重复 Close 不得 panic。
func TestAuditCloseIsIdempotent(t *testing.T) {
	writer, path := newTestAudit(t, nil)
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "a", hostSource: "ip", port: 1, up: 1, down: 1})
	writer.Close()
	writer.Close()
	// 关闭后的在途投递必须是 no-op，而不是 panic 或堆积。
	writer.submit(auditRecord{user: "u", inboundTag: "in", network: "tcp", host: "late", hostSource: "ip", port: 1, up: 1, down: 1})
	var sawStop bool
	for _, record := range readLines(t, path) {
		if record["ev"] == "stop" {
			sawStop = true
		}
		if record["host"] == "late" {
			t.Fatal("关闭后的投递不应落盘")
		}
	}
	if !sawStop {
		t.Fatal("关闭序必须写一条 stop 行")
	}
}

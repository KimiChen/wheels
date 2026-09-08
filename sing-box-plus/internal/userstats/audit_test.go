package userstats

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
)

func newTestAudit(t *testing.T, identities []string, override func(*AccessLogOptions)) (*auditWriter, string) {
	t.Helper()
	dir := shortTempDir(t)
	options := AccessLogOptions{Directory: dir}
	if err := options.normalize(); err != nil {
		t.Fatalf("规整审计配置失败：%v", err)
	}
	options.FlushIntervalMs = 20
	if override != nil {
		override(&options)
	}
	writer, err := newAuditWriter("node-test", "0123456789abcdef0123456789abcdef", options,
		identities, log.NewNOPFactory().Logger())
	if err != nil {
		t.Fatalf("创建审计 writer 失败：%v", err)
	}
	t.Cleanup(func() { writer.Close() })
	return writer, dir
}

func auditPathOf(dir string, identity string) string {
	return filepath.Join(dir, auditFileName(identity))
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

func access(user string, network string, host string, up int64, down int64) auditRecord {
	return auditRecord{user: user, inboundTag: "in", network: network, host: host,
		hostSource: "sniff", port: 443, up: up, down: down}
}

// TestAuditPerIdentitySeparation 是「数据主体分离」的核心断言：
// 每个计费身份一个文件，任何一个文件里都不出现别人的记录。
func TestAuditPerIdentitySeparation(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"alice", "bob"}, nil)
	writer.submit(access("alice", "tcp", "a.example", 10, 20))
	writer.submit(access("bob", "tcp", "b.example", 30, 40))
	writer.submit(access("alice", "tcp", "c.example", 50, 60))
	writer.Close()

	for _, item := range []struct {
		identity string
		hosts    []string
	}{
		{"alice", []string{"a.example", "c.example"}},
		{"bob", []string{"b.example"}},
	} {
		records := readLines(t, auditPathOf(dir, item.identity))
		var hosts []string
		for _, record := range records {
			if record["ev"] != nil {
				continue
			}
			if record["user"] != item.identity {
				t.Fatalf("%s 的文件里出现了别人的记录：%v", item.identity, record["user"])
			}
			hosts = append(hosts, record["host"].(string))
		}
		if strings.Join(hosts, ",") != strings.Join(item.hosts, ",") {
			t.Fatalf("%s 的记录不符：%v，期望 %v", item.identity, hosts, item.hosts)
		}
	}
	// 每个文件的 seq 独立自增，从 1 开始——文件要能被独立阅读与查缺。
	alice := readLines(t, auditPathOf(dir, "alice"))
	if alice[0]["seq"].(float64) != 1 || alice[1]["seq"].(float64) != 2 {
		t.Fatalf("seq 应逐文件自增：%v", alice)
	}
}

// TestAuditFileNameIsReversibleAndSafe 覆盖 base64url 映射的两条性质。
func TestAuditFileNameIsReversibleAndSafe(t *testing.T) {
	for _, identity := range []string{
		"alice", "u_example_01", "user@example.com",
		"../../etc/passwd", "a/b/c", ".", "..", "带中文的名字", "x-0123456789abcdef",
	} {
		name := auditFileName(identity)
		// 文件名里不得出现路径分隔符或点，否则可能穿越或产生特殊名。
		if strings.ContainsAny(strings.TrimSuffix(strings.TrimPrefix(name, auditFilePrefix), auditFileSuffix), "/.") {
			t.Fatalf("%q 的文件名含危险字符：%s", identity, name)
		}
		if filepath.Base(name) != name {
			t.Fatalf("%q 的文件名不是单一路径分量：%s", identity, name)
		}
		// 必须可逆，按人交付与按人删除都依赖这一点。
		decoded, ok := auditIdentityFromFileName(name)
		if !ok || decoded != identity {
			t.Fatalf("%q 反解失败：ok=%v got=%q", identity, ok, decoded)
		}
		// 已轮转文件同样要能归因。
		rotatedDecoded, ok := auditIdentityFromFileName(name + ".20260907T000000.000000000Z")
		if !ok || rotatedDecoded != identity {
			t.Fatalf("%q 的轮转文件反解失败", identity)
		}
	}
}

// TestAuditTraversalIdentityStaysInDirectory 是路径穿越的直接断言。
//
// §3 只要求身份名是 ASCII 可显示非空白字符，因此 `../../tmp/x` 是合法身份名。
func TestAuditTraversalIdentityStaysInDirectory(t *testing.T) {
	const evil = "../../tmp/sbp-escape"
	writer, dir := newTestAudit(t, []string{evil}, nil)
	writer.submit(access(evil, "tcp", "a.example", 1, 1))
	writer.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应只在目录内产生一个文件，实际 %d 个", len(entries))
	}
	if _, err = os.Stat("/tmp/sbp-escape"); err == nil {
		t.Fatal("身份名穿越到了目录之外")
	}
	records := readLines(t, filepath.Join(dir, entries[0].Name()))
	if records[0]["user"] != evil {
		t.Fatalf("记录里的身份名应原样保留：%v", records[0]["user"])
	}
}

// TestAuditNameCollisionFailsClosed 是开发环境的护栏：不区分大小写的文件系统上，
// 两个身份编出只差大小写的文件名会被合并进同一个文件。
func TestAuditNameCollisionFailsClosed(t *testing.T) {
	// 穷举单字节，找一对编码只差大小写的身份名。
	// 例如 "a"(0x61) 编成 "YQ"、0xc9 编成 "yQ"，两者折叠后都是 "yq"。
	var left, right string
	folded := map[string]string{}
	for value := 0; value < 256 && left == ""; value++ {
		identity := string([]byte{byte(value)})
		key := strings.ToLower(base64.RawURLEncoding.EncodeToString([]byte(identity)))
		if previous, exists := folded[key]; exists {
			left, right = previous, identity
			break
		}
		folded[key] = identity
	}
	if left == "" {
		t.Skip("未找到大小写折叠相撞的样例")
	}
	if err := checkAuditNameCollisions([]string{left, right}); err == nil {
		t.Fatalf("%q 与 %q 的文件名折叠后相同，应失败关闭", left, right)
	}
	if err := checkAuditNameCollisions([]string{"alice", "bob"}); err != nil {
		t.Fatalf("正常身份不应报错：%v", err)
	}
}

// TestAuditSuccessCriteria 是纪律 2：成功判据是 AND 不是 OR。
func TestAuditSuccessCriteria(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	writer.submit(access("u1", "tcp", "a.example", 10, 0))
	writer.submit(access("u1", "tcp", "b.example", 0, 10))
	writer.submit(access("u1", "tcp", "c.example", 5, 7))
	writer.submit(access("u1", "udp", "d.example", 3, 0))
	writer.submit(access("u1", "udp", "e.example", 0, 9))
	writer.Close()

	var hosts []string
	for _, record := range readLines(t, auditPathOf(dir, "u1")) {
		if record["ev"] == nil {
			hosts = append(hosts, record["host"].(string))
		}
	}
	if strings.Join(hosts, ",") != "c.example,d.example" {
		t.Fatalf("成功判据错误，落盘记录为：%v", hosts)
	}
}

// TestAuditSentinelSkipped 是纪律 8：载体标记地址不是访问目标。
func TestAuditSentinelSkipped(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	for host := range auditSentinelHosts {
		writer.submit(access("u1", "tcp", host, 1, 1))
	}
	writer.submit(access("u1", "tcp", "real.example", 1, 1))
	writer.Close()
	var count int
	for _, record := range readLines(t, auditPathOf(dir, "u1")) {
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

// TestAuditHostNormalization 是纪律 6、7。
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
	if strings.ContainsRune(normalizeAuditHost("a\xffb"), 0xff) {
		t.Fatal("非法 UTF-8 必须替换")
	}

	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	writer.submit(access("u1", "tcp", long, 1, 1))
	writer.Close()
	raw, _ := os.ReadFile(auditPathOf(dir, "u1"))
	if len(raw) > 4096 {
		t.Fatalf("单行长度必须有界，实际 %d 字节", len(raw))
	}
}

// TestAuditGapIsAttributedToIdentity 断言队列满的 gap 落进**那个人**的文件。
//
// 队列满时我们知道是谁的记录被丢了，笼统记在一处会让另一个人的文件出现与他无关的缺口。
func TestAuditGapIsAttributedToIdentity(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"noisy", "quiet"}, func(o *AccessLogOptions) {
		o.QueueSize = 1
		o.FlushIntervalMs = 10_000
	})
	// 先让 quiet 的记录进入队列并被写走，否则它也会被洪泛挤掉——
	// 那样这条用例就证明不了「gap 落在丢弃方而不是所有人」。
	writer.submit(access("quiet", "tcp", "y.example", 1, 1))
	time.Sleep(80 * time.Millisecond)
	for index := 0; index < 64; index++ {
		writer.submit(access("noisy", "tcp", "x.example", 1, 1))
	}
	writer.Close()

	var noisyGap, quietGap int
	for _, record := range readLines(t, auditPathOf(dir, "noisy")) {
		if record["ev"] == "gap" {
			noisyGap++
			if record["reason"] != "queue_full" || record["n"] == nil {
				t.Fatalf("gap 行内容不符：%v", record)
			}
		}
	}
	if path := auditPathOf(dir, "quiet"); fileExists(path) {
		for _, record := range readLines(t, path) {
			if record["ev"] == "gap" {
				quietGap++
			}
		}
	}
	if noisyGap == 0 {
		t.Fatal("丢弃方的文件里应有 gap 行")
	}
	if quietGap != 0 {
		t.Fatal("没有丢弃的身份不应出现 gap 行")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestAuditWriteFailureResets 是纪律 3。
func TestAuditWriteFailureResets(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	writer.submit(access("u1", "tcp", "a.example", 1, 1))
	time.Sleep(60 * time.Millisecond)
	writer.onWriterGoroutine(func() {
		file := writer.files["u1"]
		if file == nil {
			t.Error("文件应已打开")
			return
		}
		_ = file.file.Close()
		writer.write(file, []byte("{\"x\":1}\n"))
		writer.flush(file)
		if writer.droppedWrite.Load() == 0 {
			t.Error("写失败必须累加 dropped_write 并复位 writer")
		}
		writer.reopen(file)
		before := writer.droppedWrite.Load()
		writer.write(file, []byte("{\"z\":3}\n"))
		writer.flush(file)
		if writer.droppedWrite.Load() != before {
			t.Error("复位并重开之后不应继续计入写失败")
		}
	})
	_ = dir
}

// TestAuditRepairTrailingNewline 是纪律 5。
func TestAuditRepairTrailingNewline(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, auditFileName("u1"))
	if err := os.WriteFile(path, []byte("{\"seq\":1}\n{\"seq\":2,\"half\""), 0o600); err != nil {
		t.Fatalf("写入半行失败：%v", err)
	}
	options := AccessLogOptions{Directory: dir}
	if err := options.normalize(); err != nil {
		t.Fatalf("规整失败：%v", err)
	}
	writer, err := newAuditWriter("n", "r", options, []string{"u1"}, log.NewNOPFactory().Logger())
	if err != nil {
		t.Fatalf("创建 writer 失败：%v", err)
	}
	writer.submit(access("u1", "tcp", "x.example", 1, 1))
	writer.Close()
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines[1] != "{\"seq\":2,\"half\"" {
		t.Fatalf("半行应原样保留并单独成行：%q", lines[1])
	}
	if !json.Valid([]byte(lines[2])) {
		t.Fatalf("补残行失败，新记录与半行粘连：%q", lines[2])
	}
}

// TestAuditRotationDetection 是纪律 9。
func TestAuditRotationDetection(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	path := auditPathOf(dir, "u1")
	writer.submit(access("u1", "tcp", "a.example", 1, 1))
	time.Sleep(60 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除文件失败：%v", err)
	}
	writer.submit(access("u1", "tcp", "b.example", 1, 1))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !fileExists(path) {
		t.Fatal("文件被外部删除后必须重开，不能继续写进已删除 inode")
	}
	writer.Close()
}

// TestAuditTotalCapNoSelfReferentialGap 是纪律 10：无对象可删时只累加计数、不写 gap。
func TestAuditTotalCapNoSelfReferentialGap(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, func(o *AccessLogOptions) {
		o.MaxBytes = 64 * 1024
		o.MaxTotalBytes = 64 * 1024
	})
	writer.submit(access("u1", "tcp", "a.example", 1, 1))
	time.Sleep(60 * time.Millisecond)
	// 目录里只有当前文件、没有任何已轮转文件，因此没有可删对象。
	if err := os.WriteFile(auditPathOf(dir, "u1"),
		[]byte(strings.Repeat("{\"seq\":0}\n", 16*1024)), 0o600); err != nil {
		t.Fatalf("撑大文件失败：%v", err)
	}
	writer.onWriterGoroutine(func() {
		file := writer.files["u1"]
		if file == nil {
			t.Error("文件应已打开")
			return
		}
		beforeDropped := writer.droppedWrite.Load()
		beforeSeq := file.seq
		writer.enforceTotalCap()
		if writer.droppedWrite.Load() == beforeDropped {
			t.Error("无对象可删时应累加计数")
		}
		if file.seq != beforeSeq {
			t.Error("无对象可删时不得写 gap，否则自指 gap 会放大真事件")
		}
	})
	writer.Close()
}

// TestAuditMaxOpenFilesEviction 断言打开数有上限，超出时按 LRU 关闭且不丢记录。
func TestAuditMaxOpenFilesEviction(t *testing.T) {
	identities := []string{"u1", "u2", "u3", "u4"}
	writer, dir := newTestAudit(t, identities, func(o *AccessLogOptions) { o.MaxOpenFiles = 2 })
	for _, identity := range identities {
		writer.submit(access(identity, "tcp", identity+".example", 1, 1))
		time.Sleep(30 * time.Millisecond)
	}
	writer.onWriterGoroutine(func() {
		if len(writer.files) > 2 {
			t.Errorf("打开文件数应受上限约束，实际 %d", len(writer.files))
		}
	})
	writer.Close()
	for _, identity := range identities {
		records := readLines(t, auditPathOf(dir, identity))
		var found bool
		for _, record := range records {
			if record["ev"] == nil && record["host"] == identity+".example" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s 的记录在 LRU 关闭后丢失了", identity)
		}
	}
}

// TestAuditCloseWritesStopPerFile 是纪律 13：每个打开的文件各写一条 stop 行，重复 Close 不 panic。
func TestAuditCloseWritesStopPerFile(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1", "u2"}, nil)
	writer.submit(access("u1", "tcp", "a.example", 1, 1))
	writer.submit(access("u2", "tcp", "b.example", 1, 1))
	writer.Close()
	writer.Close()
	writer.submit(access("u1", "tcp", "late.example", 1, 1))
	for _, identity := range []string{"u1", "u2"} {
		var sawStop bool
		for _, record := range readLines(t, auditPathOf(dir, identity)) {
			if record["ev"] == "stop" {
				sawStop = true
			}
			if record["host"] == "late.example" {
				t.Fatal("关闭后的投递不应落盘")
			}
		}
		if !sawStop {
			t.Fatalf("%s 的文件缺少 stop 行", identity)
		}
	}
}

// TestAuditDirectoryMustExist 是启动期 fail-hard：目录不存在或是符号链接都必须拒绝。
func TestAuditDirectoryMustExist(t *testing.T) {
	base := shortTempDir(t)
	missing := AccessLogOptions{Directory: filepath.Join(base, "nope")}
	if err := missing.normalize(); err != nil {
		t.Fatalf("规整失败：%v", err)
	}
	if _, err := newAuditWriter("n", "r", missing, nil, log.NewNOPFactory().Logger()); err == nil {
		t.Fatal("目录不存在时必须启动失败")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("创建符号链接失败：%v", err)
	}
	symlinked := AccessLogOptions{Directory: link}
	if err := symlinked.normalize(); err != nil {
		t.Fatalf("规整失败：%v", err)
	}
	if _, err := newAuditWriter("n", "r", symlinked, nil, log.NewNOPFactory().Logger()); err == nil {
		t.Fatal("目录是符号链接时必须拒绝")
	}
}

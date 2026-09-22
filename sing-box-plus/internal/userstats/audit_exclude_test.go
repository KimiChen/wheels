package userstats

import (
	"os"
	"testing"
)

func TestExcludeFilterMatch(t *testing.T) {
	filter, err := newExcludeFilter(
		[]string{"github.com", "GOOGLE.com", "*.twimg.com", ".cdn-apple.com"},
		[]string{"198.18.0.0/15", "17.253.0.0/16", "1.1.1.1", "2001:db8::/32"},
		nil)
	if err != nil {
		t.Fatalf("编译排除规则失败：%v", err)
	}
	cases := []struct {
		host   string
		source string
		want   bool
		why    string
	}{
		{"github.com", auditHostSourceSniff, true, "后缀本身"},
		{"raw.githubusercontent.com", auditHostSourceSniff, false, "githubusercontent.com 不是 github.com 的子域"},
		{"api.github.com", auditHostSourceSniff, true, "子域"},
		{"WWW.Google.COM", auditHostSourceSniff, true, "大小写不敏感"},
		{"evilgithub.com", auditHostSourceSniff, false, "子串不算后缀，否则会误伤同名尾巴的域"},
		{"github.com.attacker.net", auditHostSourceSniff, false, "前缀出现不算命中"},
		{"pbs.twimg.com", auditHostSourceSniff, true, "*. 前缀规范化后等价于后缀"},
		{"cdn-apple.com", auditHostSourceSniff, true, "前导点规范化"},
		{"github.com", auditHostSourceFqdn, true, "fqdn 来源同样走域名规则"},
		{"198.18.7.112", auditHostSourceIP, true, "fake-ip 网段"},
		{"198.51.100.1", auditHostSourceIP, false, "/15 之外"},
		{"17.253.114.43", auditHostSourceIP, true, "Apple 网段"},
		{"1.1.1.1", auditHostSourceIP, true, "裸地址按 /32"},
		{"1.1.1.2", auditHostSourceIP, false, "裸地址不扩散到邻居"},
		{"2001:db8::1", auditHostSourceIP, true, "IPv6 网段"},
		{"::ffff:198.18.7.112", auditHostSourceIP, true, "v4-mapped 要先 Unmap 再比，否则 fake-ip 从 v6 表示法漏过去"},
		{"", auditHostSourceIP, false, "空 host 不匹配"},
		// 这一条钉住「规则匹配的是将要落盘的 host，不是解析结果」：
		// 域名记录永远不拿 exclude_ips 去比，哪怕它解析出来就在那个网段里。
		{"time.apple.com", auditHostSourceSniff, false, "域名不走 IP 规则"},
		{"github.com", auditHostSourceIP, false, "IP 来源不走域名规则"},
	}
	for _, item := range cases {
		if got := filter.match(item.host, item.source, 0); got != item.want {
			t.Errorf("match(%q, %q) = %v，期望 %v（%s）", item.host, item.source, got, item.want, item.why)
		}
	}
}

func TestExcludeFilterRejectsAmbiguousConfig(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		ips   []string
		ports []uint16
	}{
		{"hosts 里写 IP", []string{"1.1.1.1"}, nil, nil},
		{"hosts 里写网段", []string{"198.18.0.0/15"}, nil, nil},
		{"hosts 空项", []string{""}, nil, nil},
		{"hosts 重复", []string{"github.com", "GitHub.com"}, nil, nil},
		{"ips 空项", nil, []string{""}, nil},
		{"ips 不是地址", nil, []string{"github.com"}, nil},
		{"ips 重复", nil, []string{"1.1.1.1", "1.1.1.1/32"}, nil},
		// 带主机位的网段几乎总是位数写错了，静默 Masked() 会让生效范围远大于作者预期。
		{"网段带主机位", nil, []string{"198.18.7.1/15"}, nil},
		// 0 不是一个可连接的端口。收下它只会让人以为自己排掉了什么。
		{"ports 里写 0", nil, nil, []uint16{0}},
		{"ports 重复", nil, nil, []uint16{123, 123}},
	}
	for _, item := range cases {
		if _, err := newExcludeFilter(item.hosts, item.ips, item.ports); err == nil {
			t.Errorf("%s：应当报错，实际通过", item.name)
		}
	}
}

func TestExcludeFilterEmptyIsNil(t *testing.T) {
	filter, err := newExcludeFilter(nil, nil, nil)
	if err != nil {
		t.Fatalf("空规则不应报错：%v", err)
	}
	if !filter.empty() || filter.match("github.com", auditHostSourceSniff, 0) {
		t.Fatal("未配置排除时不得挡下任何连接")
	}
}

// TestAuditFilterStampTravelsWithFile 钉住：排除是静默的，因此生效规则必须写在文件里。
// 否则日志交付出去之后，读的人无法区分「没访问过」与「访问过但被规则挡了」。
func TestAuditFilterStampTravelsWithFile(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, func(options *AccessLogOptions) {
		options.ExcludeHosts = []string{"github.com"}
		options.ExcludeIPs = []string{"198.18.0.0/15"}
	})
	writer.submit(access("u1", "tcp", "example.com", 10, 20))
	writer.Close()

	records := readLines(t, auditPathOf(dir, "u1"))
	if len(records) == 0 || records[0]["ev"] != "filter" {
		t.Fatalf("文件首行应是 filter 留痕，实际：%v", records)
	}
	hosts, _ := records[0]["hosts"].([]any)
	ips, _ := records[0]["ips"].([]any)
	if len(hosts) != 1 || hosts[0] != "github.com" || len(ips) != 1 || ips[0] != "198.18.0.0/15" {
		t.Fatalf("留痕内容与配置不符：%v", records[0])
	}
	if records[0]["seq"].(float64) != 1 || records[1]["seq"].(float64) != 2 {
		t.Fatalf("留痕要占 seq，跳过它会让下游连续性校验失效：%v", records)
	}
}

func TestAuditNoStampWhenNoFilter(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, nil)
	writer.submit(access("u1", "tcp", "example.com", 1, 1))
	writer.Close()
	for _, record := range readLines(t, auditPathOf(dir, "u1")) {
		if record["ev"] == "filter" {
			t.Fatalf("未配置排除时不该出现 filter 行：%v", record)
		}
	}
}

// TestAuditExcludeRotatedFileAlsoStamped：轮转后的新文件同样要带规则，
// 否则只有第一个文件说得清自己被过滤过。
func TestAuditExcludeRotatedFileAlsoStamped(t *testing.T) {
	writer, dir := newTestAudit(t, []string{"u1"}, func(options *AccessLogOptions) {
		options.ExcludeHosts = []string{"github.com"}
		options.MaxBytes = 64 * 1024
	})
	for i := 0; i < 400; i++ {
		writer.submit(access("u1", "tcp", "example.com", 1024, 4096))
	}
	writer.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("样本不足以触发轮转：%d 个文件", len(entries))
	}
	for _, entry := range entries {
		records := readLines(t, dir+"/"+entry.Name())
		if len(records) == 0 || records[0]["ev"] != "filter" {
			t.Fatalf("%s 首行应是 filter 留痕：%v", entry.Name(), records)
		}
	}
}

// 端口排除与另外两条的关键差别：它**不以 host 为对象**。
func TestExcludeFilterPorts(t *testing.T) {
	filter, err := newExcludeFilter(nil, nil, []uint16{123, 4460})
	if err != nil {
		t.Fatalf("编译排除规则失败：%v", err)
	}
	cases := []struct {
		name   string
		host   string
		source string
		port   uint16
		want   bool
	}{
		{"域名 + 命中端口", "pool.ntp.org", auditHostSourceSniff, 123, true},
		{"地址 + 命中端口", "91.189.91.112", auditHostSourceIP, 123, true},
		// **这一条是端口规则存在的理由之一**：一条既没嗅探出域名、也拿不到
		// 地址的连接，hosts 与 ips 都无从判起，而它的目的端口仍然是确定的。
		{"host 为空 + 命中端口", "", auditHostSourceIP, 123, true},
		{"NTS 密钥交换", "time.cloudflare.com", auditHostSourceSniff, 4460, true},
		{"没命中的端口", "pool.ntp.org", auditHostSourceSniff, 443, false},
		{"host 为空且端口没命中", "", auditHostSourceIP, 443, false},
	}
	for _, item := range cases {
		if got := filter.match(item.host, item.source, item.port); got != item.want {
			t.Errorf("%s：期望 %v，实际 %v", item.name, item.want, got)
		}
	}
}

// describe 进的是文件头那行留痕，下游靠它判断「规则变过没有」。
// map 的遍历顺序每次都不同，所以**必须排序**——不排的话，两份规则完全
// 相同的日志会写出两行看着不一样的 filter 事件。
func TestExcludeFilterDescribeSortsPorts(t *testing.T) {
	filter, err := newExcludeFilter(nil, nil, []uint16{4460, 123, 1900})
	if err != nil {
		t.Fatalf("编译排除规则失败：%v", err)
	}
	for i := 0; i < 20; i++ {
		_, _, ports := filter.describe()
		if len(ports) != 3 || ports[0] != 123 || ports[1] != 1900 || ports[2] != 4460 {
			t.Fatalf("第 %d 次 describe 顺序不对：%v", i, ports)
		}
	}
}

// 只配端口时过滤器不能被当成空的——空会让它整个不生效。
func TestExcludeFilterPortsOnlyIsNotEmpty(t *testing.T) {
	filter, err := newExcludeFilter(nil, nil, []uint16{123})
	if err != nil {
		t.Fatalf("编译排除规则失败：%v", err)
	}
	if filter == nil {
		t.Fatal("只配端口时返回了 nil，规则会整个失效")
	}
	if filter.empty() {
		t.Fatal("只配端口时被判成空")
	}
}

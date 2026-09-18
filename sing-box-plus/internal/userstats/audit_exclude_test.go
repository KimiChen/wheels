package userstats

import (
	"os"
	"testing"
)

func TestExcludeFilterMatch(t *testing.T) {
	filter, err := newExcludeFilter(
		[]string{"github.com", "GOOGLE.com", "*.twimg.com", ".cdn-apple.com"},
		[]string{"198.18.0.0/15", "17.253.0.0/16", "1.1.1.1", "2001:db8::/32"})
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
		if got := filter.match(item.host, item.source); got != item.want {
			t.Errorf("match(%q, %q) = %v，期望 %v（%s）", item.host, item.source, got, item.want, item.why)
		}
	}
}

func TestExcludeFilterRejectsAmbiguousConfig(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		ips   []string
	}{
		{"hosts 里写 IP", []string{"1.1.1.1"}, nil},
		{"hosts 里写网段", []string{"198.18.0.0/15"}, nil},
		{"hosts 空项", []string{""}, nil},
		{"hosts 重复", []string{"github.com", "GitHub.com"}, nil},
		{"ips 空项", nil, []string{""}},
		{"ips 不是地址", nil, []string{"github.com"}},
		{"ips 重复", nil, []string{"1.1.1.1", "1.1.1.1/32"}},
		// 带主机位的网段几乎总是位数写错了，静默 Masked() 会让生效范围远大于作者预期。
		{"网段带主机位", nil, []string{"198.18.7.1/15"}},
	}
	for _, item := range cases {
		if _, err := newExcludeFilter(item.hosts, item.ips); err == nil {
			t.Errorf("%s：应当报错，实际通过", item.name)
		}
	}
}

func TestExcludeFilterEmptyIsNil(t *testing.T) {
	filter, err := newExcludeFilter(nil, nil)
	if err != nil {
		t.Fatalf("空规则不应报错：%v", err)
	}
	if !filter.empty() || filter.match("github.com", auditHostSourceSniff) {
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

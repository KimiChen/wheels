package userstats

import (
	"net/netip"
	"sort"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
)

// excludeFilter 决定一条连接**是否值得写进审计**。
//
// 它只作用在审计这条旁路上。计数器与配额扣减在 connState.countUplink/countDownlink 里
// 无条件执行，排除规则够不到它们——被排掉的流量照样计费、照样扣额度，
// 只是审计里不再有对应的行（README §4.8 排除规则一节）。
//
// 匹配的对象是**将要写进记录的 host 字段**，不是路由最终解析出的地址：
// 嗅探到域名的连接永远走 hosts 规则，即使它解析出来的 IP 命中了 ips 里的网段；
// ips 规则只能命中 host_src=ip、也就是压根没有域名可写的那些记录。
// 这样规则与文件内容一一对应，读日志的人不必去猜当时的 DNS 结果。
type excludeFilter struct {
	// suffixes 已规范化：小写、无前导点。命中条件是 host == d 或 host 以 "."+d 结尾，
	// 不做子串匹配——"evilgithub.com" 不该被 "github.com" 命中。
	suffixes []string
	prefixes []netip.Prefix
	// ports 按**目的端口**排，与 host 无关。
	//
	// 存在的理由是 hosts/ips 排不掉的那一类：NTP 客户端从一个轮询的池子里取
	// 服务器，每次拿到的 IP 都不同——按地址排是排不完的，今天列 53 条，
	// 明天换成另外 53 条。而它们的共同点只有端口。
	//
	// **它比另外两条钝得多**：排掉一个端口就是排掉走那个端口的一切，
	// 与目标是谁无关。排 443 等于让审计整个失明。配置由人写、随部署提交，
	// 这里只校验形状不猜意图——但这句话要在配置那一侧也写着。
	ports map[uint16]struct{}
}

func newExcludeFilter(hosts []string, ips []string, ports []uint16) (*excludeFilter, error) {
	filter := &excludeFilter{}
	seenHost := make(map[string]struct{}, len(hosts))
	for _, item := range hosts {
		entry := strings.ToLower(strings.TrimSpace(item))
		entry = strings.TrimPrefix(entry, "*.")
		entry = strings.Trim(entry, ".")
		if entry == "" {
			return nil, E.New("user_stats.access_log.exclude_hosts 出现空项")
		}
		if strings.ContainsAny(entry, "/ \t") {
			return nil, E.New("user_stats.access_log.exclude_hosts 只接受域名后缀，网段请写进 exclude_ips：", item)
		}
		// IP 字面量写进 hosts 是无效配置而不是等价写法：审计里 IP 记录的 host
		// 不会以 "." 分级，后缀匹配对它没有意义。失败得响一点，别让人以为已经排掉了。
		if _, err := netip.ParseAddr(entry); err == nil {
			return nil, E.New("user_stats.access_log.exclude_hosts 不接受 IP 字面量，请写进 exclude_ips：", item)
		}
		if _, duplicate := seenHost[entry]; duplicate {
			return nil, E.New("user_stats.access_log.exclude_hosts 出现重复项：", entry)
		}
		seenHost[entry] = struct{}{}
		filter.suffixes = append(filter.suffixes, entry)
	}
	seenPrefix := make(map[string]struct{}, len(ips))
	for _, item := range ips {
		entry := strings.TrimSpace(item)
		if entry == "" {
			return nil, E.New("user_stats.access_log.exclude_ips 出现空项")
		}
		prefix, err := parseExcludePrefix(entry)
		if err != nil {
			return nil, err
		}
		key := prefix.String()
		if _, duplicate := seenPrefix[key]; duplicate {
			return nil, E.New("user_stats.access_log.exclude_ips 出现重复项：", key)
		}
		seenPrefix[key] = struct{}{}
		filter.prefixes = append(filter.prefixes, prefix)
	}
	seenPort := make(map[uint16]struct{}, len(ports))
	for _, port := range ports {
		// 0 不是一个可连接的端口。收下它只会让人以为自己排掉了什么。
		if port == 0 {
			return nil, E.New("user_stats.access_log.exclude_ports 不接受 0")
		}
		if _, duplicate := seenPort[port]; duplicate {
			return nil, E.New("user_stats.access_log.exclude_ports 出现重复项：", port)
		}
		seenPort[port] = struct{}{}
	}
	if len(seenPort) > 0 {
		filter.ports = seenPort
	}
	if filter.empty() {
		return nil, nil
	}
	return filter, nil
}

// parseExcludePrefix 接受 CIDR 与裸地址两种写法，裸地址按 /32、/128 处理。
//
// 带主机位的 CIDR（198.18.7.1/15）一律报错而不是静默 Masked()：那通常是写错了位数，
// 静默取整会让实际生效的范围比作者以为的大得多。
func parseExcludePrefix(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, E.Cause(err, "解析 user_stats.access_log.exclude_ips 项 ", entry)
		}
		if prefix.Masked() != prefix {
			return netip.Prefix{}, E.New(
				"user_stats.access_log.exclude_ips 的网段带了主机位：", entry,
				"，请写成 ", prefix.Masked().String())
		}
		return prefix, nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, E.Cause(err, "解析 user_stats.access_log.exclude_ips 项 ", entry)
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func (f *excludeFilter) empty() bool {
	return f == nil || (len(f.suffixes) == 0 && len(f.prefixes) == 0 && len(f.ports) == 0)
}

// match 在每条连接建立时各调一次，不在字节热路径上。
//
// 规则条数是配置量级（十几条），线性扫描比建索引更便宜也更好读。
func (f *excludeFilter) match(host string, source string, port uint16) bool {
	if f.empty() {
		return false
	}
	// **端口先判，而且不要求 host 非空。** 另外两条都以 host 为对象，
	// 所以它们在 host 为空时无从判起；端口不是——一条没嗅探出域名、
	// 也拿不到地址的连接，它的目的端口仍然是确定的。
	if _, hit := f.ports[port]; hit {
		return true
	}
	if host == "" {
		return false
	}
	if source == auditHostSourceIP {
		addr, err := netip.ParseAddr(host)
		if err != nil {
			return false
		}
		addr = addr.Unmap()
		for _, prefix := range f.prefixes {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	lowered := strings.ToLower(host)
	for _, suffix := range f.suffixes {
		if lowered == suffix || strings.HasSuffix(lowered, "."+suffix) {
			return true
		}
	}
	return false
}

// describe 供文件头的 filter 事件行使用：把生效规则随数据一起留痕。
func (f *excludeFilter) describe() (hosts []string, ips []string, ports []uint16) {
	if f.empty() {
		return nil, nil, nil
	}
	hosts = append(hosts, f.suffixes...)
	for _, prefix := range f.prefixes {
		ips = append(ips, prefix.String())
	}
	for port := range f.ports {
		ports = append(ports, port)
	}
	// **排序。** 它进的是文件头那行留痕，而 map 的遍历顺序每次都不同——
	// 不排序的话，两份规则完全相同的日志会写出两行看着不一样的 filter 事件，
	// 而下游要靠这行判断「规则变过没有」。
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return hosts, ips, ports
}

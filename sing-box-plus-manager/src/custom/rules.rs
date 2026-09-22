//! 个人自定义分流规则。
//!
//! 一批「这个域名 / 这个网段走我自己那一档」，渲染成一个 `Rules-<登录名>`
//! 代理组，加一批**排在最前面**的 Clash 规则。
//!
//! # 排在最前面是这个功能能不能用的前提
//!
//! Clash 是**首条匹配即生效**。内建那 262 条已经把 `anthropic.com`、
//! `github.com` 这类全匹配走了——自定义规则排在它们后面的话，
//! 「我想让 claude.ai 走自己的节点」会完全不起作用，
//! 而用户看到的是「规则加了但没反应」，一个没有任何报错的失败。
//!
//! 所以它们插在 `rules:` 的**第一条**。代价写在页面上：
//! 写错一条能把已有分流搞乱。
//!
//! # 规则类型是闭集
//!
//! 只收五种。Clash 支持的远不止这些（PROCESS-NAME、GEOIP、RULE-SET…），
//! 但每多一种就多一类校验不到的输入——`GEOIP,CN` 写错成 `GEOIP,ZH`
//! 不会报错，只会永远不匹配。五种覆盖「加个 ip 或者域名」这句话，
//! 要更多时再逐个加，每加一种连着加它的校验。

use crate::error::{Error, Result};
use serde::{Deserialize, Serialize};

/// 一个人最多多少条规则。
///
/// **有上限而不是无限**：规则整份进每一次订阅渲染，而订阅是客户端定时拉的。
/// 一千条域名会让每份订阅多出几十 KB，且那几十 KB 每次都重新渲染一遍。
/// 200 条足够表达「我特别关心的那些站点」，不够用说明该用规则集而不是逐条。
pub const MAX_RULES: usize = 200;

fn invalid(message: impl Into<String>) -> Error {
    Error::Codec(message.into())
}

/// 规则类型。**闭集**，与页面上的下拉、与 `as_clash()` 一起改。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum RuleKind {
    /// 匹配这个域名及其全部子域。最常用的一种。
    DomainSuffix,
    /// 只匹配这一个域名，子域不匹配。
    Domain,
    /// 域名里含这个子串就匹配。**最容易误伤**，页面上要说清。
    DomainKeyword,
    IpCidr,
    IpCidr6,
}

impl RuleKind {
    /// Clash 规则里的类型名。
    pub fn as_clash(self) -> &'static str {
        match self {
            RuleKind::DomainSuffix => "DOMAIN-SUFFIX",
            RuleKind::Domain => "DOMAIN",
            RuleKind::DomainKeyword => "DOMAIN-KEYWORD",
            RuleKind::IpCidr => "IP-CIDR",
            RuleKind::IpCidr6 => "IP-CIDR6",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Rule {
    pub kind: RuleKind,
    pub value: String,
}

/// 这个人的整份规则表。**顺序即优先级**。
pub type RuleSet = Vec<Rule>;

/// 那个代理组在订阅里叫什么。
///
/// `Rules-` 前缀与自定义上游的 `Custom-`、SOCKS5 的 `Socks5-` 分属三个
/// 命名空间，所以它不会与任何一条自定义节点撞名——这是使用者定的名字，
/// 而它恰好把一类冲突从根上消掉了。
pub fn group_name(login_name: &str) -> String {
    format!("Rules-{login_name}")
}

/// 校验并规范化一条规则。
pub fn normalize(kind: RuleKind, value: &str) -> Result<Rule> {
    let value = value.trim();
    if value.is_empty() {
        return Err(invalid("规则内容不能为空"));
    }
    if value.chars().any(|c| (c as u32) < 0x20 || c as u32 == 0x7f) {
        return Err(invalid("规则内容包含控制字符"));
    }
    // **逗号会把一条规则拆成两条。** Clash 的规则是逗号分隔的三段
    // （类型,内容,目标），内容里混进逗号就会让目标错位——
    // 而错位之后那条规则指向的是一个不存在的组，Clash 拒绝整份配置。
    if value.contains(',') {
        return Err(invalid("规则内容不能含逗号"));
    }
    let normalized = match kind {
        RuleKind::DomainSuffix | RuleKind::Domain => {
            // 复用节点那套域名校验：同样拒非 ASCII 并提示 punycode。
            crate::custom::parse::host(value)?
        }
        RuleKind::DomainKeyword => {
            if value.len() > 128 || !value.is_ascii() {
                return Err(invalid("关键字过长或含非 ASCII 字符"));
            }
            // 关键字太短会误伤到几乎所有域名。两个字符的关键字
            // （比如 `co`）会把 `.com` 全部命中。
            if value.len() < 3 {
                return Err(invalid("关键字至少 3 个字符，太短会误伤几乎所有域名"));
            }
            value.to_ascii_lowercase()
        }
        RuleKind::IpCidr => cidr(value, false)?,
        RuleKind::IpCidr6 => cidr(value, true)?,
    };
    Ok(Rule { kind, value: normalized })
}

/// 网段。**必须已经按位数取整**，带主机位的一律报错。
///
/// 静默取整（`192.168.1.5/16` → `192.168.0.0/16`）会让实际生效的范围
/// 比作者以为的大得多——与节点侧 `exclude_ips` 是同一条纪律。
fn cidr(value: &str, want_v6: bool) -> Result<String> {
    let (addr_text, bits_text) =
        value.split_once('/').ok_or_else(|| invalid("网段要写成 地址/位数 的形式"))?;
    let addr: std::net::IpAddr =
        addr_text.parse().map_err(|_| invalid(format!("「{addr_text}」不是合法 IP 地址")))?;
    let bits: u32 = bits_text.parse().map_err(|_| invalid("位数必须是数字"))?;
    let (is_v6, max_bits) = match addr {
        std::net::IpAddr::V4(_) => (false, 32u32),
        std::net::IpAddr::V6(_) => (true, 128u32),
    };
    if is_v6 != want_v6 {
        return Err(invalid(if want_v6 {
            "IP-CIDR6 要填 IPv6 网段"
        } else {
            "IP-CIDR 要填 IPv4 网段"
        }));
    }
    if bits > max_bits {
        return Err(invalid(format!("位数不能超过 {max_bits}")));
    }
    if masked(addr, bits) != addr {
        return Err(invalid(format!("网段带了主机位，请写成 {}/{bits}", masked(addr, bits))));
    }
    Ok(format!("{addr}/{bits}"))
}

fn masked(addr: std::net::IpAddr, bits: u32) -> std::net::IpAddr {
    match addr {
        std::net::IpAddr::V4(v4) => {
            let raw = u32::from(v4);
            let mask = if bits == 0 { 0 } else { u32::MAX << (32 - bits) };
            std::net::IpAddr::V4((raw & mask).into())
        }
        std::net::IpAddr::V6(v6) => {
            let raw = u128::from(v6);
            let mask = if bits == 0 { 0 } else { u128::MAX << (128 - bits) };
            std::net::IpAddr::V6((raw & mask).into())
        }
    }
}

/// 整份规则表的校验：条数上限 + **同类型同内容不许重复**。
///
/// 重复不会让 Clash 报错，它只是让后一条永远不生效——
/// 而用户会以为自己改了什么，其实改的是一条死规则。
pub fn validate_set(rules: &RuleSet) -> Result<()> {
    if rules.len() > MAX_RULES {
        return Err(invalid(format!("规则最多 {MAX_RULES} 条，当前 {}", rules.len())));
    }
    let mut seen = std::collections::BTreeSet::new();
    for rule in rules {
        if !seen.insert((rule.kind.as_clash(), rule.value.as_str())) {
            return Err(invalid(format!(
                "「{} {}」重复了——重复的那条永远不会生效",
                rule.kind.as_clash(),
                rule.value
            )));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 域名规则() {
        let rule = normalize(RuleKind::DomainSuffix, " Example.COM ").unwrap();
        assert_eq!(rule.value, "example.com", "要 trim 并小写");
        assert_eq!(rule.kind.as_clash(), "DOMAIN-SUFFIX");
    }

    /// **逗号会把一条规则拆成两条**，让目标错位、指向一个不存在的组，
    /// 而 Clash 对此是拒绝整份配置。
    #[test]
    fn 拒绝逗号() {
        assert!(normalize(RuleKind::DomainSuffix, "a.com,DIRECT").is_err());
        assert!(normalize(RuleKind::DomainKeyword, "ab,cd").is_err());
    }

    #[test]
    fn 关键字太短要拒() {
        // `co` 会把 .com 全部命中。
        assert!(normalize(RuleKind::DomainKeyword, "co").is_err());
        assert!(normalize(RuleKind::DomainKeyword, "google").is_ok());
    }

    #[test]
    fn 网段必须取整() {
        assert_eq!(normalize(RuleKind::IpCidr, "10.0.0.0/8").unwrap().value, "10.0.0.0/8");
        let error = normalize(RuleKind::IpCidr, "192.168.1.5/16").unwrap_err();
        // 报错里要给出**正确的写法**，不能只说「错了」。
        assert!(format!("{error}").contains("192.168.0.0/16"), "{error}");
    }

    #[test]
    fn 网段类型要对得上() {
        assert!(normalize(RuleKind::IpCidr, "2001:db8::/32").is_err(), "v6 填进 IP-CIDR 该拒");
        assert!(normalize(RuleKind::IpCidr6, "10.0.0.0/8").is_err(), "v4 填进 IP-CIDR6 该拒");
        assert!(normalize(RuleKind::IpCidr6, "2001:db8::/32").is_ok());
    }

    #[test]
    fn 位数越界要拒() {
        assert!(normalize(RuleKind::IpCidr, "10.0.0.0/33").is_err());
        assert!(normalize(RuleKind::IpCidr, "10.0.0.0/abc").is_err());
        assert!(normalize(RuleKind::IpCidr, "10.0.0.0").is_err(), "缺位数该拒");
    }

    #[test]
    fn 重复的规则要拒() {
        let rules = vec![
            normalize(RuleKind::DomainSuffix, "a.com").unwrap(),
            normalize(RuleKind::DomainSuffix, "a.com").unwrap(),
        ];
        let error = validate_set(&rules).unwrap_err();
        assert!(format!("{error}").contains("永远不会生效"), "{error}");
    }

    /// 类型不同就不算重复——`DOMAIN,a.com` 与 `DOMAIN-SUFFIX,a.com` 是两回事。
    #[test]
    fn 类型不同不算重复() {
        let rules = vec![
            normalize(RuleKind::Domain, "a.com").unwrap(),
            normalize(RuleKind::DomainSuffix, "a.com").unwrap(),
        ];
        assert!(validate_set(&rules).is_ok());
    }

    #[test]
    fn 条数有上限() {
        let one = normalize(RuleKind::DomainSuffix, "a.com").unwrap();
        let mut rules: RuleSet = Vec::new();
        for i in 0..=MAX_RULES {
            rules.push(Rule { value: format!("h{i}.example"), ..one.clone() });
        }
        assert!(validate_set(&rules).is_err());
    }

    #[test]
    fn 组名带rules前缀() {
        assert_eq!(group_name("kimi"), "Rules-kimi");
    }

    #[test]
    fn 规则往返json() {
        let rules = vec![
            normalize(RuleKind::DomainSuffix, "a.com").unwrap(),
            normalize(RuleKind::IpCidr, "10.0.0.0/8").unwrap(),
        ];
        let text = serde_json::to_string(&rules).unwrap();
        let back: RuleSet = serde_json::from_str(&text).unwrap();
        assert_eq!(rules, back);
    }
}

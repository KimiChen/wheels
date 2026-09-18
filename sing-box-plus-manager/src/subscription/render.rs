//! 把一个人的身份 + 入口清单渲染成客户端能吃的订阅（README §4.7）。
//!
//! 出的是 **Clash / mihomo YAML**，而且**模板直接取自旧站**
//! （`operations` 仓 `docs/paoyouProxy/proxy.paoyou.work/resources/subscription/base.yaml`）。
//!
//! 为什么搬旧的而不是自己写一份最小配置：那 290 行规则是**积累出来的**——
//! 海外 AI 的域名清单、国内直连的判据、TUN 与 DNS 的那一套参数，
//! 每一条背后都是一次「为什么这个网站走错了路」。重写一份「先能用」的，
//! 等于把这些全扔掉，然后在接下来几个月里一条一条重新踩回来。
//!
//! 搬过来的是 **head**（mixed-port / TUN / DNS / sniffer / geodata）与
//! **rules**（三个组的分流规则），逐字节不动。
//! **换掉的只有 `proxies:` 与 `proxy-groups:`**：前者按这个人的凭据与入口生成，
//! 后者按配置里的组定义生成——旧模板里那 8 个节点名指向的是旧 SS 那套端口
//! （64902-64905、19999、64911-64913），与新拓扑对不上，照抄会得到一份
//! 引用了不存在节点的配置，而 Clash 对此的报错是「订阅格式错误」。

use crate::config::{Credentials, Entry};

/// 旧站模板的两半，构建时嵌入。**逐字节不动。**
const HEAD: &str = include_str!("../../web/subscription-head.yaml");
const RULES: &str = include_str!("../../web/subscription-rules.yaml");

/// 订阅正文：Clash / mihomo YAML。
///
/// `head + proxies + proxy-groups + rules`，中间两段是生成的。
pub fn clash_yaml(
    entries: &[Entry],
    groups: &[crate::config::ProxyGroup],
    credentials: &Credentials,
    identity: &str,
    profile_name: &str,
) -> String {
    let Some(upsk) = credentials.upsk.get(identity) else {
        // 身份有、凭据没有：发一份**明说的空配置**，不是编一个密码。
        // 编一个的后果是用户导入后连不上，而客户端只会说「握手失败」。
        return format!(
            "# {profile_name}\n\
             # 这个账号还没有分配到节点，或者它的凭据不在主控的凭据文件里。\n\
             # 请联系管理员——**不要**反复重新导入，重新导入不会改变这一点。\n\
             mixed-port: 7890\nmode: rule\nproxies: []\nproxy-groups: []\n\
             rules:\n  - MATCH,DIRECT\n"
        );
    };
    // SS2022 带 EIH 时，Clash 的 password 是 `<iPSK>:<uPSK>`。
    let password = format!("{}:{}", credentials.ipsk, upsk);

    let mut out = String::with_capacity(HEAD.len() + RULES.len() + 4096);
    out.push_str(&format!("# {profile_name}\n"));
    out.push_str("# 由 sing-box-plus-manager 生成。节点与密码随本人账号绑定，**请勿转发**。\n");
    out.push_str(HEAD);

    out.push_str("\nproxies:\n");
    for entry in entries {
        out.push_str(&format!(
            "  - name: {}\n    type: ss\n    server: {}\n    port: {}\n    cipher: {}\n    password: {}\n    udp: true\n",
            yaml_string(&entry.name),
            yaml_string(&entry.host),
            entry.port,
            yaml_string(&credentials.method),
            yaml_string(&password),
        ));
    }

    out.push_str("\nproxy-groups:\n");
    for group in groups {
        out.push_str(&format!("  - name: {}\n", yaml_string(&group.name)));
        if let Some(icon) = &group.icon {
            out.push_str(&format!("    icon: {}\n", yaml_string(icon)));
        }
        out.push_str("    type: select\n    proxies:\n");
        for member in &group.proxies {
            out.push_str(&format!("      - {}\n", yaml_string(member)));
        }
    }

    out.push('\n');
    out.push_str(RULES);
    out
}

/// YAML 标量：一律双引号并转义。
///
/// 不做「看起来安全就不加引号」的判断：密码是 base64，含 `+` `/` `=`，
/// 而 `=` 开头在 YAML 里有特殊含义；节点名可能含中文与空格。
/// 少加一次引号的代价是整份配置解析失败，而客户端只说「订阅格式错误」。
fn yaml_string(value: &str) -> String {
    let escaped = value.replace('\\', "\\\\").replace('"', "\\\"");
    format!("\"{escaped}\"")
}

/// `Content-Disposition` 的两个文件名。
///
/// **响应头绝不含 token。** 生产教训写在 §4.7：最初拿 opaque token 当文件名，
/// 客户端导入后显示成一串乱码；等发现要改名时，已导入的旧订阅还得让每个人
/// 手动删除重导。所以名字来自**已授权用户的账号名**。
///
/// 返回 `(ascii_fallback, rfc5987)`。ASCII 那份是给不认 `filename*` 的老客户端的。
pub fn disposition(profile_name: &str) -> (String, String) {
    let ascii: String = profile_name
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || c == '-' || c == '_' { c } else { '_' })
        .collect();
    let ascii = if ascii.is_empty() { "subscription".to_string() } else { ascii };
    (format!("{ascii}.yaml"), format!("UTF-8''{}.yaml", percent_encode(profile_name)))
}

/// 订阅的显示名：`Proxy-<账号名>`。
///
/// 它同时是代理组名、`MATCH` 的目标、以及 `Content-Disposition` 的文件名——
/// 三处必须是同一个串，否则客户端里显示的名字与配置里的组名对不上。
pub const PROFILE_PREFIX: &str = "Proxy-";

pub fn profile_name(login_name: &str) -> String {
    format!("{PROFILE_PREFIX}{login_name}")
}

/// `Subscription-Userinfo`：Clash / mihomo 靠它显示流量与到期。
///
/// 四个字段都必须是**十进制**。`total` 取真实额度（§4.7 明确警告不要填假值——
/// 本项目有真实闸断，假值会让客户端显示与实际行为对不上）；
/// `upload`/`download` 取**当前周期**的已结算累计，不是 lifetime。
pub fn userinfo(upload: u128, download: u128, total: u64, expire_unix: i64) -> String {
    format!("upload={upload}; download={download}; total={total}; expire={expire_unix}")
}

fn percent_encode(value: &str) -> String {
    let mut out = String::with_capacity(value.len() * 3);
    for byte in value.as_bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            out.push(*byte as char);
        } else {
            out.push_str(&format!("%{byte:02X}"));
        }
    }
    out
}

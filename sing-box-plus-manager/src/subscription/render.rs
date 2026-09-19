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

use crate::config::{Credentials, Transport};

/// 旧站模板的两半，构建时嵌入。**逐字节不动。**
const HEAD: &str = include_str!("../../web/subscription-head.yaml");
const RULES: &str = include_str!("../../web/subscription-rules.yaml");

/// 订阅正文：Clash / mihomo YAML。
///
/// `head + proxies + proxy-groups + rules`，中间两段是生成的。
pub fn clash_yaml(
    config: &crate::config::SubscriptionConfig,
    source: &crate::config::EntrySource,
    credentials: &Credentials,
    identity: &str,
    profile_name: &str,
    visible: &std::collections::BTreeSet<&str>,
) -> String {
    // **级别不够的节点整条入口不出现**（`quota::level`）。
    //
    // 只管出不出现，不是拦截：四台上同一个身份用的是同一份凭据。
    let entries: Vec<&crate::config::Entry> =
        config.entries.iter().filter(|entry| visible.contains(entry.node_id.as_str())).collect();
    // **组成员必须跟着一起过滤。** 代理组里写的是入口名，过滤掉入口却留着组员，
    // Clash 会因为「组里引用了不存在的节点」**拒绝整份配置**——
    // 低等级用户拿到的就不是「少几个节点」，而是完全不能用，
    // 而客户端只说一句「订阅格式错误」。
    let kept: std::collections::BTreeSet<&str> =
        entries.iter().map(|entry| entry.name.as_str()).collect();
    let groups = &config.groups;
    // 这种拨法要的凭据在不在。SS 要 uPSK，VLESS 要 UUID——**分开判**：
    // 凭据文件是分两次长出来的（uuid 那一半 2026-09-20 才加），
    // 一份只有 uPSK 的旧文件对内网那份订阅完全够用，不该连带把它也打成空配置。
    let has_credential = match source.transport {
        Transport::Ss => credentials.upsk.contains_key(identity),
        Transport::Vless => credentials.uuid.contains_key(identity),
    };
    let upsk = credentials.upsk.get(identity).map(String::as_str).unwrap_or_default();
    if !has_credential {
        // 身份有、凭据没有：发一份**明说的空配置**，不是编一个密码。
        // 编一个的后果是用户导入后连不上，而客户端只会说「握手失败」。
        return format!(
            "# {profile_name}\n\
             # 这个账号还没有分配到节点，或者它的凭据不在主控的凭据文件里。\n\
             # 请联系管理员——**不要**反复重新导入，重新导入不会改变这一点。\n\
             mixed-port: 7890\nmode: rule\nproxies: []\nproxy-groups: []\n\
             rules:\n  - MATCH,DIRECT\n"
        );
    }
    // SS2022 带 EIH 时，Clash 的 password 是 `<iPSK>:<uPSK>`。
    let password = format!("{}:{}", credentials.ipsk, upsk);

    let mut out = String::with_capacity(HEAD.len() + RULES.len() + 4096);
    out.push_str(&format!("# {profile_name}\n"));
    out.push_str("# 由 sing-box-plus-manager 生成。节点与密码随本人账号绑定，**请勿转发**。\n");
    out.push_str(HEAD);

    out.push_str("\nproxies:\n");
    for entry in &entries {
        // **入口名逐字复用，两种协议一模一样。** 代理组的成员写的就是这些名字，
        // 名字一变三个组与两份规则集模板都要跟着改；复用之后
        // `proxy-groups` 与 `rules` 两段在两份订阅里**逐字节相同**。
        let Some(port) = entry.port_for(source.transport) else {
            // 配置校验已经保证它存在。走到这里说明门禁漏了，
            // 而这里绝不替它编一个端口——编一个的后果是发出一份连不上的订阅。
            continue;
        };
        out.push_str(&format!("  - name: {}\n", yaml_string(&entry.name)));
        out.push_str(&format!("    server: {}\n", yaml_string(config.host_for(entry, source))));
        out.push_str(&format!("    port: {port}\n"));
        match source.transport {
            Transport::Ss => {
                out.push_str("    type: ss\n");
                out.push_str(&format!("    cipher: {}\n", yaml_string(&credentials.method)));
                out.push_str(&format!("    password: {}\n", yaml_string(&password)));
                out.push_str("    udp: true\n");
            }
            Transport::Vless => {
                // 走到这里 vless 与 reality 一定在：`validate()` 校验过
                // 「有 vless 来源就必须有 [subscription.vless]」与
                // 「每条入口的 node_id 都要有一套 REALITY 参数」。
                let (Some(vless), Some(uuid)) = (&config.vless, credentials.uuid.get(identity))
                else {
                    continue;
                };
                let Some(reality) = vless.reality.get(&entry.node_id) else { continue };
                out.push_str("    type: vless\n");
                out.push_str(&format!("    uuid: {}\n", yaml_string(uuid)));
                out.push_str("    network: tcp\n");
                out.push_str("    udp: true\n");
                out.push_str("    tls: true\n");
                out.push_str(&format!("    flow: {}\n", yaml_string(&vless.flow)));
                out.push_str(&format!(
                    "    packet-encoding: {}\n",
                    yaml_string(&vless.packet_encoding)
                ));
                out.push_str(&format!("    servername: {}\n", yaml_string(&vless.servername)));
                out.push_str(&format!(
                    "    client-fingerprint: {}\n",
                    yaml_string(&vless.client_fingerprint)
                ));
                out.push_str("    reality-opts:\n");
                out.push_str(&format!("      public-key: {}\n", yaml_string(&reality.public_key)));
                // **short-id 必须加引号。** 它是十六进制，纯数字的那些（`01234567`）
                // 不加引号会被 YAML 当成整数、前导零一并丢掉，
                // 而客户端对此只有一句「握手失败」。
                out.push_str(&format!("      short-id: {}\n", yaml_string(&reality.short_id)));
            }
        }
    }

    out.push_str("\nproxy-groups:\n");
    for group in groups {
        out.push_str(&format!("  - name: {}\n", yaml_string(&group.name)));
        if let Some(icon) = &group.icon {
            out.push_str(&format!("    icon: {}\n", yaml_string(icon)));
        }
        out.push_str("    type: select\n    proxies:\n");
        let mut written = 0;
        for member in &group.proxies {
            // 入口名要在可见集合里；内置目标与组名原样放行
            // （`validate` 已经保证组员只可能是这三类之一）。
            let is_entry = config.entries.iter().any(|entry| entry.name == *member);
            if is_entry && !kept.contains(member.as_str()) {
                continue;
            }
            out.push_str(&format!("      - {}\n", yaml_string(member)));
            written += 1;
        }
        // **空组同样会让 Clash 拒绝整份配置。** 三个组现在都带 DIRECT，
        // 所以走不到这里；但「现在都带」不是约束，加一个纯节点组就会走到。
        if written == 0 {
            out.push_str("      - \"DIRECT\"\n");
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

/// 订阅的显示名：`<前缀><账号名>`，例如 `proxyWan-kimi`。
///
/// 它同时是**三样东西**：客户端里显示的名字、`Content-Disposition` 的文件名、
/// 以及订阅正文首行的注释。三处必须是同一个串，否则客户端里显示的名字与
/// 地址对不上，而人会以为自己导错了订阅。
///
/// 前缀同时也出现在订阅地址里，所以一眼就能对上「这份是哪种拨法」——
/// 两份订阅同时导进一个客户端时，这一点是唯一能区分它们的东西。
pub fn profile_name(login_name: &str, source: &crate::config::EntrySource) -> String {
    format!("{}{login_name}", source.prefix)
}

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

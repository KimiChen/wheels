//! `server.toml` 的 `[subscription]` 段：订阅出口与凭据来源（README §4.7）。
//!
//! **凭据不进数据库。** 它们放在一个单独的文件里，按 mtime 重读——
//! 与成员名单同一条纪律，理由也相似但不完全一样：
//!
//! * 名单放文件是因为**撤权不能等**；
//! * 凭据放文件是因为**它们不该进账本的备份**。账本要按 R3 做一致性备份并演练恢复，
//!   而那份备份会被复制到别处；uPSK 进去之后，每一份账本备份都变成一份凭据副本。
//!
//! 凭据的真正来源在部署侧（`identity-pool.json`）。主控读的是一份**派生文件**，
//! 不是直接读那个格式——跨仓契约 §2.3 的同一条：消费样例，不要抄对方的形状。

use std::collections::BTreeMap;
use std::path::PathBuf;

use serde::Deserialize;

use crate::error::{Error, Result};

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SubscriptionConfig {
    /// 订阅链接的对外基址。`<基址>/sub/<token>`。
    ///
    /// 与 `sso.public_base_url` 分开写：订阅端点按 §4.7 应当**分开监听、分开限流**，
    /// 将来挪到另一个域名或端口时，改这一处就够了。
    pub public_base_url: String,

    /// 凭据文件。0600，属主是主控服务账号。
    pub credentials_path: PathBuf,

    /// 入口的**来源**：同一批端口有几种到达方式。
    ///
    /// 这批入口是入口机上的 NAT 转发（`fib daddr type local`），
    /// 对**任意本机地址**生效——也就是说内网地址与公网地址走的是同一条规则、
    /// 同一个端口、同一份凭据，**差别只有客户端拨哪个 host**。
    /// 所以这里不是「两套入口」，是一套入口的两种到达方式；
    /// 把 7 条入口抄两遍只会让它们在某次改端口时分家。
    pub sources: Vec<EntrySource>,

    /// 入口清单。顺序即订阅里节点的顺序。
    pub entries: Vec<Entry>,

    /// 代理组。名字与成员都要与旧站模板里的规则集对得上——
    /// 规则里写的是 `…,海外AI` 这样的组名，组不存在会让整份配置解析失败。
    pub groups: Vec<ProxyGroup>,

    /// VLESS 那一侧的公共参数。只要有一个来源是 `transport = "vless"` 就必须有。
    #[serde(default)]
    pub vless: Option<VlessConfig>,
}

/// 客户端拨这条入口时说的是哪种协议。
///
/// **两种来源不再只差一个 host。** 内网继续用 SS，公网用 VLESS——SS 容易被墙。
/// 于是「一套入口的两种拨法」这句话只剩一半还对：端口、凭据、token 仍然共用，
/// 协议不再共用。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Transport {
    Ss,
    Vless,
}

impl Transport {
    /// `Entry.ports` 里的键，也是 `server.toml` 里写的那个字面量。
    pub fn key(self) -> &'static str {
        match self {
            Transport::Ss => "ss",
            Transport::Vless => "vless",
        }
    }
}

/// VLESS + REALITY 的公共参数。逐字段都会进订阅正文。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VlessConfig {
    /// REALITY 的握手目标域名，客户端侧的 `servername`。
    pub servername: String,
    /// `client-fingerprint`，例如 `chrome`。
    pub client_fingerprint: String,
    /// per-user 的 `flow`，例如 `xtls-rprx-vision`。
    pub flow: String,
    /// `packet-encoding`，例如 `xudp`。
    pub packet_encoding: String,
    /// **按 node_id 索引的** REALITY 公开参数。每台节点一套密钥
    /// （与 mTLS leaf 同惯例：一台被攻陷不牵连其余三台），而 7 条入口里
    /// 有 3 条分别终结在不同节点上，`Entry` 本来就带 node_id，所以对得上。
    pub reality: BTreeMap<String, Reality>,
}

/// 一台节点的 REALITY 公开参数。**这里只放公开的那一半**——
/// 私钥在部署侧的 `private/network-reality/<短名>.json`，不进主控。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Reality {
    pub public_key: String,
    pub short_id: String,
}

/// 一个代理组。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProxyGroup {
    pub name: String,
    #[serde(default)]
    pub icon: Option<String>,
    /// 成员。可以是入口名，也可以是 `DIRECT` / `REJECT` 这类内置目标。
    pub proxies: Vec<String>,
}

/// 一种到达方式。
///
/// 同一批入口在入口机上是 NAT 转发（`fib daddr type local`），对**任意本机地址**生效——
/// 内网地址与公网地址走的是同一条规则、同一个端口、同一份凭据、同一个 token，
/// **差别只有客户端拨哪个 host**。所以这里不是「两套入口」，
/// 是一套入口的两种拨法；把入口清单抄两遍只会让它们在某次改端口时分家。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EntrySource {
    /// 订阅文件名的前缀，例如 `proxyWan-`。
    ///
    /// 它同时是**三样东西**：URL 里那一段、客户端里的显示名、
    /// 以及 `Content-Disposition` 的文件名。三处必须是同一个串，
    /// 否则客户端里显示的名字与地址对不上，而人会以为自己导错了。
    pub prefix: String,
    /// 客户端要拨的地址。
    pub host: String,
    /// 给人看的一句话，出现在控制台上（例如「公司内网」）。不进任何 URL。
    #[serde(default)]
    pub note: String,
    /// 这种拨法用哪种协议。**没有默认值，必须显式写。**
    /// 给一个默认（比如 ss）的话，「忘了把公网那份标成 vless」会是完全无声的：
    /// 配置照常加载，订阅照常发出，只是公网那份发的是一份会被墙的 SS。
    pub transport: Transport,
}

/// 一条入口。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Entry {
    /// 显示名。会出现在客户端的节点列表里。
    pub name: String,
    /// **覆盖**来源的 host。一般不填——填了就表示这条入口只有一种到达方式，
    /// 它会在每一份订阅里都用这个地址。
    #[serde(default)]
    pub host: Option<String>,
    /// 协议 → 端口。键是 `Transport::key()`。
    ///
    /// **一条入口一行、每种协议一个端口**，而不是把清单抄两遍。抄两遍的下场是
    /// 它们在某次改端口时分家，而两份订阅里的节点名一模一样，看不出来。
    /// 编号规则：VLESS 口 = 对应的 SS 口 + 100。
    pub ports: BTreeMap<String, u16>,
    /// 这条入口最终落到哪个节点。用来在订阅里标注出口，也用来将来按节点过滤。
    pub node_id: String,
}

impl Entry {
    /// 这条入口在这种协议下的端口。配置校验保证它存在，所以调用方拿到
    /// `None` 就是配置门禁漏了——不要在渲染时替它编一个。
    pub fn port_for(&self, transport: Transport) -> Option<u16> {
        self.ports.get(transport.key()).copied()
    }
}

impl SubscriptionConfig {
    /// 某个来源的订阅地址：`<基址>/sub/<前缀><token>.yaml`。
    ///
    /// **两种拨法共用同一个 token**，差的只是前缀——它们是同一份订阅的两种拨法，
    /// 不是两份订阅。吊销一次两条一起失效，这正是想要的。
    ///
    /// 带扩展名不是装饰：部分客户端按扩展名决定怎么解析；
    /// 而前缀让人在浏览器历史或聊天记录里一眼看出这是什么——
    /// 也因此**别把它发进任何群**（§4.7、D10）。
    pub fn subscription_url(&self, token: &str, source: &EntrySource) -> String {
        format!("{}/sub/{}{token}.yaml", self.public_base_url.trim_end_matches('/'), source.prefix)
    }

    /// 从订阅文件名认出是哪一种拨法。认不出就是 `None`——调用方回 404。
    pub fn source_of<'a, 'n>(&'a self, file_name: &'n str) -> Option<(&'a EntrySource, &'n str)> {
        self.sources.iter().find_map(|source| {
            let rest = file_name.strip_prefix(source.prefix.as_str())?;
            let token = rest.strip_suffix(".yaml")?;
            (token.len() == 64 && token.bytes().all(|b| b.is_ascii_hexdigit()))
                .then_some((source, token))
        })
    }

    /// 这条入口在这种拨法下要拨的地址。
    pub fn host_for<'a>(&self, entry: &'a Entry, source: &'a EntrySource) -> &'a str {
        entry.host.as_deref().unwrap_or(&source.host)
    }

    pub fn validate(&self) -> Result<()> {
        if !self.public_base_url.starts_with("https://") {
            return Err(Error::invalid_config(
                "§4.7",
                "subscription.public_base_url 必须是 https://：订阅链接就是凭据",
            ));
        }
        if self.sources.is_empty() {
            return Err(Error::invalid_config(
                "§4.7",
                "subscription.sources 为空：没有来源就不知道客户端该拨哪个地址",
            ));
        }
        let mut prefixes: Vec<&str> = Vec::new();
        for source in &self.sources {
            if source.host.is_empty() {
                return Err(Error::invalid_config(
                    "§4.7",
                    format!("subscription.sources 里 {:?} 的 host 是空的", source.prefix),
                ));
            }
            // 前缀会进 URL 路径的最后一段。限死字符集不是洁癖：带 `/` 或 `.` 的前缀
            // 会让订阅地址指到别的地方去，而那条路径**不认会话、只认 token**。
            if source.prefix.is_empty()
                || !source.prefix.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
            {
                return Err(Error::invalid_config(
                    "§4.7",
                    format!(
                        "subscription.sources 的 prefix {:?} 只能是字母、数字与连字符",
                        source.prefix
                    ),
                ));
            }
            // **不允许一个前缀是另一个的前缀。** 那样同一个文件名会匹配上两种拨法，
            // 而匹配到哪一个取决于配置顺序——一个静默的、改配置就会变的答案。
            for other in &prefixes {
                if source.prefix.starts_with(other) || other.starts_with(&source.prefix) {
                    return Err(Error::invalid_config(
                        "§4.7",
                        format!(
                            "前缀 {:?} 与 {other:?} 互为前缀：同一个订阅文件名会匹配上两种拨法，\
                             而结果取决于配置顺序",
                            source.prefix
                        ),
                    ));
                }
            }
            prefixes.push(&source.prefix);
        }

        if self.entries.is_empty() {
            return Err(Error::invalid_config(
                "§4.7",
                "subscription.entries 为空：那会发出一份没有任何节点的订阅，\
                 而客户端导入它之后看起来是「成功了」",
            ));
        }
        let mut seen = std::collections::BTreeSet::new();
        for entry in &self.entries {
            if entry.name.is_empty() || entry.ports.is_empty() {
                return Err(Error::invalid_config("§4.7", "subscription.entries 里有空字段"));
            }
            for (protocol, port) in &entry.ports {
                if *port == 0 {
                    return Err(Error::invalid_config(
                        "§4.7",
                        format!("入口 {:?} 的 {protocol} 端口是 0", entry.name),
                    ));
                }
            }
            if entry.host.as_deref().is_some_and(str::is_empty) {
                return Err(Error::invalid_config(
                    "§4.7",
                    format!("入口 {:?} 的 host 写成了空串。不覆盖来源就整个不写这一行", entry.name),
                ));
            }
            // 名称在客户端里是主键：重名会让其中一条静默消失，
            // 而用户看到的是「少了一个节点」，查不出原因。
            if !seen.insert(entry.name.as_str()) {
                return Err(Error::invalid_config(
                    "§4.7",
                    format!("入口名 {:?} 重复：客户端按名字去重，重名会让一条静默消失", entry.name),
                ));
            }
        }
        // **每个来源的协议都要被每条入口覆盖。** 少一个的后果不是报错：
        // 那种拨法下这条入口会从订阅里消失，而客户端只表现成「少了一个节点」。
        for source in &self.sources {
            for entry in &self.entries {
                if entry.port_for(source.transport).is_none() {
                    return Err(Error::invalid_config(
                        "§4.7",
                        format!(
                            "入口 {:?} 没有 {} 端口，而来源 {:?} 是这种协议",
                            entry.name,
                            source.transport.key(),
                            source.prefix
                        ),
                    ));
                }
            }
        }

        // 只要有一种拨法是 VLESS，公共参数就必须在。
        let needs_vless = self.sources.iter().any(|s| s.transport == Transport::Vless);
        match (&self.vless, needs_vless) {
            (None, true) => {
                return Err(Error::invalid_config(
                    "§4.7",
                    "有来源是 transport = \"vless\"，但没有 [subscription.vless] 段",
                ));
            }
            (Some(vless), _) => {
                if vless.servername.is_empty()
                    || vless.client_fingerprint.is_empty()
                    || vless.flow.is_empty()
                    || vless.packet_encoding.is_empty()
                {
                    return Err(Error::invalid_config(
                        "§4.7",
                        "[subscription.vless] 有空字段：每一项都会原样进订阅正文",
                    ));
                }
                // **每条入口的落点都要有一套 REALITY 参数。**
                // 少配一台节点的后果是那条入口发出去连不上，而客户端只说
                // 「握手失败」——所以这里要的是**启动就失败**，不是发一份坏订阅。
                for entry in &self.entries {
                    let Some(reality) = vless.reality.get(&entry.node_id) else {
                        return Err(Error::invalid_config(
                            "§4.7",
                            format!(
                                "入口 {:?} 落在 {}，而 [subscription.vless.reality] 里没有这一台",
                                entry.name, entry.node_id
                            ),
                        ));
                    };
                    if reality.public_key.is_empty() || reality.short_id.is_empty() {
                        return Err(Error::invalid_config(
                            "§4.7",
                            format!("节点 {} 的 REALITY 参数有空字段", entry.node_id),
                        ));
                    }
                }
            }
            (None, false) => {}
        }

        // 组成员必须存在。写错一个名字的后果是 Clash 整份配置解析失败，
        // 而它只说「订阅格式错误」——指不到这里。
        let names: std::collections::BTreeSet<&str> =
            self.entries.iter().map(|e| e.name.as_str()).collect();
        const BUILTIN: &[&str] = &["DIRECT", "REJECT", "PASS", "COMPATIBLE"];
        let group_names: std::collections::BTreeSet<&str> =
            self.groups.iter().map(|g| g.name.as_str()).collect();
        for group in &self.groups {
            // 组名与节点名同一个命名空间，撞了就成了指向自己的组。
            if names.contains(group.name.as_str()) {
                return Err(Error::invalid_config(
                    "§4.7",
                    format!("代理组名 {:?} 与某个入口同名：客户端里会成为指向自己的组", group.name),
                ));
            }
            for member in &group.proxies {
                if !names.contains(member.as_str())
                    && !BUILTIN.contains(&member.as_str())
                    && !group_names.contains(member.as_str())
                {
                    return Err(Error::invalid_config(
                        "§4.7",
                        format!("代理组 {:?} 里的 {member:?} 既不是入口也不是内置目标", group.name),
                    ));
                }
            }
        }
        Ok(())
    }
}

/// 凭据文件的内容。
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Credentials {
    /// Shadowsocks 加密方式，例如 `2022-blake3-aes-128-gcm`。
    pub method: String,
    /// 入口共享的 iPSK。
    pub ipsk: String,
    /// 身份名 → uPSK。
    pub upsk: BTreeMap<String, String>,
    /// 身份名 → VLESS UUID。
    ///
    /// **`default` 不是宽容，是推送顺序的保护。** 二进制与凭据文件是两次推送，
    /// 顺序只有两种，代价差得很远：
    ///
    /// * 先二进制、后凭据 → 新二进制读到旧文件，uuid 为空，公网订阅暂时发不出
    ///   VLESS，**内网 SS 完全不受影响**；
    /// * 先凭据、后二进制 → 本结构体带 `deny_unknown_fields`，旧二进制**整份拒绝**，
    ///   `credentials_unreadable`，`/sub/…` 对**所有人**回 503。
    ///
    /// 所以推送顺序是「二进制在前、凭据在后」，而这个 `default` 是它的安全网。
    #[serde(default)]
    pub uuid: BTreeMap<String, String>,
}

impl Credentials {
    pub fn validate(&self) -> Result<()> {
        if self.method.is_empty() || self.ipsk.is_empty() {
            return Err(Error::invalid_config("§4.7", "凭据文件缺 method 或 ipsk"));
        }
        if self.upsk.is_empty() {
            return Err(Error::invalid_config(
                "§4.7",
                "凭据文件里一个 uPSK 都没有：那会让每个人的订阅都是空的",
            ));
        }
        // **要么整个没有，要么与 uPSK 的身份集合完全一致。**
        // 「有一半」是一份渲染到一半的文件，症状是**一部分人**的公网订阅悄悄
        // 没有节点——比所有人都没有难查得多。
        if !self.uuid.is_empty() {
            let upsk: std::collections::BTreeSet<&str> =
                self.upsk.keys().map(String::as_str).collect();
            let uuid: std::collections::BTreeSet<&str> =
                self.uuid.keys().map(String::as_str).collect();
            if upsk != uuid {
                let only_upsk: Vec<&str> = upsk.difference(&uuid).copied().take(4).collect();
                let only_uuid: Vec<&str> = uuid.difference(&upsk).copied().take(4).collect();
                return Err(Error::invalid_config(
                    "§4.7",
                    format!(
                        "凭据文件的 uuid 与 upsk 身份集合不一致：只有 uPSK 的 {only_upsk:?}，\
                         只有 UUID 的 {only_uuid:?}。要么整份没有 uuid，要么两边逐个对上",
                    ),
                ));
            }
        }
        Ok(())
    }
}

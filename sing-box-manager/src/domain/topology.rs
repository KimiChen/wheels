//! 拓扑领域类型：Entry / Node / Landing / Route / RouteHop 与相关枚举。
//! 枚举 `as_str`/`parse` 严格对齐 0001_init.sql 的 CHECK 字面量。DTO 无任何 PSK 字段。

use serde::Serialize;

macro_rules! str_enum {
    ($(#[$m:meta])* $name:ident { $($variant:ident => $lit:literal),+ $(,)? }) => {
        $(#[$m])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq)]
        pub enum $name { $($variant),+ }
        impl $name {
            pub fn as_str(self) -> &'static str {
                match self { $(Self::$variant => $lit),+ }
            }
            pub fn parse(s: &str) -> Option<Self> {
                match s { $($lit => Some(Self::$variant),)+ _ => None }
            }
        }
    };
}

str_enum!(
    /// Entry 入站协议（`entries.inbound_kind`）。
    InboundKind { Shadowsocks => "shadowsocks", VlessReality => "vless-reality" }
);
str_enum!(
    /// Route 出口类型（`routes.exit_kind`）。
    ExitKind { EntryDirect => "entry_direct", Node => "node", Landing => "landing" }
);
str_enum!(
    /// Landing 类型（`landings.kind`）。
    LandingKind { ManagedNode => "managed_node", Socks5 => "socks5" }
);
str_enum!(
    /// 网络能力（`landings.network`）。
    Network { Tcp => "tcp", Udp => "udp", Both => "both" }
);
str_enum!(
    /// Route 状态（`routes.status`）。
    RouteStatus { Draft => "draft", Active => "active", Disabled => "disabled" }
);

/// 固定端口（§2.1）。
pub const ENTRY_PORT: i64 = 19736;
pub const NODE_PORT: i64 = 29736;

#[derive(Debug, Clone, Serialize)]
pub struct Entry {
    pub id: String,
    pub host_id: String,
    pub public_address: String,
    pub port: i64,
    pub inbound_kind: String,
    pub ss_method: Option<String>,
    pub allow_direct: bool,
    pub current_revision: Option<i64>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Node {
    pub id: String,
    pub host_id: String,
    pub data_address: String,
    pub port: i64,
    pub allow_direct_exit: bool,
    pub current_revision: Option<i64>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Landing {
    pub id: String,
    pub kind: String,
    pub node_id: Option<String>,
    pub socks5_address: Option<String>,
    pub socks5_port: Option<i64>,
    pub network: String,
    pub auth_credential_id: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Route {
    pub id: String,
    pub label: String,
    pub entry_id: String,
    pub exit_kind: String,
    pub exit_node_id: Option<String>,
    pub exit_landing_id: Option<String>,
    pub status: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct RouteHop {
    pub position: i64,
    pub node_id: String,
}

/// Route 创建/校验输入（有序 hops）。
#[derive(Debug, Clone)]
pub struct RouteDraft {
    pub id: Option<String>, // 更新时排除自身的 label 唯一性检查
    pub label: String,
    pub entry_id: String,
    pub hops: Vec<String>, // 有序 node_id
    pub exit_kind: ExitKind,
    pub exit_node_id: Option<String>,
    pub exit_landing_id: Option<String>,
}

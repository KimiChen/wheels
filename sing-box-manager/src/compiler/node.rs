//! Node 配置：SS-2022 inbound :29736 → direct outbound。恒 IN→direct，天然兼容「同一 Node 既中继又终端」。

use serde_json::{json, Value};

use crate::compiler::psk::NODE_SS_METHOD;
use crate::domain::topology::{Node, NODE_PORT};

pub fn compile_node(_node: &Node, node_psk: &str) -> Value {
    json!({
        "log": {"level": "warn"},
        "dns": {"servers": [{"tag": "local", "type": "local"}], "final": "local"},
        "inbounds": [{
            "type": "shadowsocks",
            "tag": "in-relay",
            "listen": "::",
            "listen_port": NODE_PORT,
            "method": NODE_SS_METHOD,
            "password": node_psk,
        }],
        "outbounds": [{"type": "direct", "tag": "direct"}],
        "route": {"rules": [{"action": "sniff"}], "final": "direct"},
    })
}

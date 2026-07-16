//! detour 出站链构建（忠实移植 legacy build_chain）。沿 [hops by position] ++ 终端 逐跳建 SS-2022 中继，
//! dedup 键 (node_id, prev_id)，tag = `out-{node}-via-{prev}`（稳定 id）；socks5 终端建 socks 出站。
//! 返回该 Route 的路由目标 outbound tag（entry_direct → "direct"）。

use std::collections::HashMap;

use serde_json::{json, Value};

use crate::compiler::psk::NODE_SS_METHOD;
use crate::compiler::{RouteSnapshot, Terminal};
use crate::domain::topology::NODE_PORT;
use crate::error::{AppError, ErrorCode, Result};
use crate::store::secrets::SecretBundle;

pub(crate) fn build_chain(
    rs: &RouteSnapshot,
    secrets: &SecretBundle,
    entry_id: &str,
    outbounds: &mut Vec<Value>,
    made: &mut HashMap<(String, String), String>,
) -> Result<String> {
    let mut prev_tag = String::from("direct");
    let mut prev_id = entry_id.to_string();

    // 需要建 SS-2022 中继的节点：全部 hops，且 Node 型终端追加为最后一跳。
    let mut relays: Vec<&crate::domain::topology::Node> = rs.hops.iter().collect();
    if let Terminal::Node(n) = &rs.terminal {
        relays.push(n);
    }
    for node in relays {
        let key = (node.id.clone(), prev_id.clone());
        let tag = if let Some(t) = made.get(&key) {
            t.clone()
        } else {
            let t = format!("out-{}-via-{}", node.id, prev_id);
            let psk = secrets.node_psk.get(&node.id).ok_or_else(|| {
                AppError::new(ErrorCode::Internal, format!("缺 node_psk: {}", node.id))
            })?;
            outbounds.push(json!({
                "type": "shadowsocks",
                "tag": t,
                "server": node.data_address,
                "server_port": NODE_PORT,
                "method": NODE_SS_METHOD,
                "password": psk,
                "detour": prev_tag,
            }));
            made.insert(key, t.clone());
            t
        };
        prev_tag = tag;
        prev_id = node.id.clone();
    }

    match &rs.terminal {
        Terminal::Direct => Ok("direct".to_string()),
        Terminal::Node(_) => Ok(prev_tag), // 最后一跳中继即出口
        Terminal::Socks5(landing) => {
            let key = (format!("socks:{}", landing.id), prev_id.clone());
            if let Some(t) = made.get(&key) {
                return Ok(t.clone());
            }
            let t = format!("out-socks-{}-via-{}", landing.id, prev_id);
            let mut ob = json!({
                "type": "socks",
                "tag": t,
                "server": landing.socks5_address,
                "server_port": landing.socks5_port,
                "version": "5",
                "detour": prev_tag,
            });
            if let Some((user, pass)) = secrets.landing_auth.get(&landing.id) {
                ob["username"] = json!(user);
                ob["password"] = json!(pass);
            }
            // 网络能力非 both 时透传 outbound.network（legacy 自测证 network:tcp 可运行）。
            if landing.network != "both" {
                ob["network"] = json!(landing.network);
            }
            outbounds.push(ob);
            made.insert(key, t.clone());
            Ok(t)
        }
    }
}

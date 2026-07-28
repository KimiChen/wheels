//! manifest → SQLite 的幂等同步。只管理清单明确声明的资源；不删除清单外对象。
//! active Route 的授权撤销需要流量结算屏障，因此本层拒绝直接撤销。

use std::collections::{BTreeMap, BTreeSet};

use serde::Serialize;
use sqlx::{Row, SqlitePool};

use super::Manifest;
use crate::crypto::Cipher;
use crate::domain::host::Capability;
use crate::domain::topology::{ExitKind, InboundKind, RouteDraft, ENTRY_PORT, NODE_PORT};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::{self, topology};

#[derive(Debug, Default, Serialize)]
pub struct ApplyReport {
    pub hosts_created: usize,
    pub hosts_updated: usize,
    pub entries_created: usize,
    pub entries_updated: usize,
    pub nodes_created: usize,
    pub nodes_updated: usize,
    pub routes_created: usize,
    pub routes_updated: usize,
    pub users_created: usize,
    pub users_updated: usize,
    pub grants_created: usize,
    pub grants_revoked: usize,
    pub subscription_tokens: Vec<NewSubscriptionToken>,
    pub entry_ids: Vec<String>,
    pub revisions_checked: Vec<String>,
    pub deployments_succeeded: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct NewSubscriptionToken {
    pub user: String,
    pub token: String,
}

pub async fn apply(pool: &SqlitePool, cipher: &Cipher, manifest: &Manifest) -> Result<ApplyReport> {
    manifest.validate()?;
    let mut report = ApplyReport::default();

    let entry_servers: BTreeSet<String> = manifest
        .listeners
        .iter()
        .map(|l| l.server.clone())
        .collect();
    let node_servers: BTreeSet<String> = manifest
        .relays
        .iter()
        .flat_map(|r| r.chain.iter().cloned())
        .collect();

    let mut host_ids = BTreeMap::new();
    for server_name in manifest.servers.keys() {
        let mut caps = Vec::new();
        if entry_servers.contains(server_name) {
            caps.push(Capability::Entry);
        }
        if node_servers.contains(server_name) {
            caps.push(Capability::Node);
        }
        let existing = sqlx::query_scalar::<_, String>("SELECT id FROM hosts WHERE name=?")
            .bind(server_name)
            .fetch_optional(pool)
            .await?;
        let host_id = match existing {
            Some(id) => {
                for cap in &caps {
                    sqlx::query(
                        "INSERT OR IGNORE INTO host_capabilities(host_id,capability) VALUES(?,?)",
                    )
                    .bind(&id)
                    .bind(cap.as_str())
                    .execute(pool)
                    .await?;
                }
                report.hosts_updated += 1;
                id
            }
            None => {
                report.hosts_created += 1;
                store::hosts::create_host(pool, server_name, Some("managed-by=manifest"), &caps)
                    .await?
            }
        };
        let address = &manifest.servers[server_name].address;
        let mgmt = host_port(address, 39736);
        store::agents::upsert_agent(pool, &host_id, &mgmt).await?;
        host_ids.insert(server_name.clone(), host_id);
    }

    let mut listener_entries = BTreeMap::new();
    for listener in &manifest.listeners {
        let host_id = &host_ids[&listener.server];
        let existing = sqlx::query_scalar::<_, String>("SELECT id FROM entries WHERE host_id=?")
            .bind(host_id)
            .fetch_optional(pool)
            .await?;
        let kind = match listener.protocol.as_str() {
            "shadowsocks" => InboundKind::Shadowsocks,
            "vless" => InboundKind::VlessReality,
            _ => unreachable!("manifest.validate 已限制协议"),
        };
        let address = &manifest.servers[&listener.server].address;
        let method = if kind == InboundKind::Shadowsocks {
            manifest
                .protocols
                .shadowsocks
                .as_ref()
                .map(|s| s.method.as_str())
        } else {
            None
        };
        let entry_id = match existing {
            Some(id) => {
                sqlx::query(
                    "UPDATE entries SET public_address=?,port=?,inbound_kind=?,ss_method=?,updated_at=? WHERE id=?",
                )
                .bind(address)
                .bind(ENTRY_PORT)
                .bind(kind.as_str())
                .bind(method)
                .bind(store::now_unix())
                .bind(&id)
                .execute(pool)
                .await?;
                report.entries_updated += 1;
                id
            }
            None => {
                report.entries_created += 1;
                topology::create_entry(
                    pool,
                    cipher,
                    &topology::NewEntry {
                        host_id,
                        public_address: address,
                        inbound_kind: kind,
                        ss_method: method,
                        allow_direct: false,
                    },
                )
                .await?
            }
        };
        if kind == InboundKind::VlessReality {
            let vless = manifest
                .protocols
                .vless
                .as_ref()
                .expect("manifest.validate 已要求 VLESS 模板");
            store::reality::ensure(
                pool,
                cipher,
                &entry_id,
                &vless.flow,
                &vless.server_name,
                &vless.handshake_server,
                i64::from(vless.handshake_port),
                &vless.client_fingerprint,
            )
            .await?;
        }
        listener_entries.insert(listener.name.clone(), entry_id.clone());
        report.entry_ids.push(entry_id);
    }
    report.entry_ids.sort();
    report.entry_ids.dedup();

    let mut node_ids = BTreeMap::new();
    for server_name in &node_servers {
        let host_id = &host_ids[server_name];
        let address = &manifest.servers[server_name].address;
        let existing = sqlx::query_scalar::<_, String>("SELECT id FROM nodes WHERE host_id=?")
            .bind(host_id)
            .fetch_optional(pool)
            .await?;
        let node_id = match existing {
            Some(id) => {
                sqlx::query(
                    "UPDATE nodes SET data_address=?,port=?,allow_direct_exit=1,updated_at=? WHERE id=?",
                )
                .bind(address)
                .bind(NODE_PORT)
                .bind(store::now_unix())
                .bind(&id)
                .execute(pool)
                .await?;
                report.nodes_updated += 1;
                id
            }
            None => {
                report.nodes_created += 1;
                topology::create_node(pool, cipher, host_id, address, true).await?
            }
        };
        node_ids.insert(server_name.clone(), node_id);
    }

    let mut route_ids = BTreeMap::new();
    for relay in &manifest.relays {
        let entry_id = &listener_entries[&relay.listener];
        let chain: Vec<String> = relay.chain.iter().map(|s| node_ids[s].clone()).collect();
        let exit_node_id = chain.last().cloned().expect("validate 非空");
        let hops = chain[..chain.len() - 1].to_vec();
        let existing =
            sqlx::query("SELECT id,entry_id,exit_node_id,status FROM routes WHERE label=?")
                .bind(&relay.name)
                .fetch_optional(pool)
                .await?;
        let route_id = match existing {
            Some(row) => {
                let id: String = row.get("id");
                let current_hops = topology::route_hops(pool, &id)
                    .await?
                    .into_iter()
                    .map(|h| h.node_id)
                    .collect::<Vec<_>>();
                let changed = row.get::<String, _>("entry_id") != *entry_id
                    || row.get::<Option<String>, _>("exit_node_id") != Some(exit_node_id.clone())
                    || current_hops != hops;
                if changed {
                    let mut tx = pool.begin().await?;
                    sqlx::query(
                        "UPDATE routes SET entry_id=?,exit_kind='node',exit_node_id=?,exit_landing_id=NULL,status='draft',updated_at=? WHERE id=?",
                    )
                    .bind(entry_id)
                    .bind(&exit_node_id)
                    .bind(store::now_unix())
                    .bind(&id)
                    .execute(&mut *tx)
                    .await?;
                    sqlx::query("DELETE FROM route_hops WHERE route_id=?")
                        .bind(&id)
                        .execute(&mut *tx)
                        .await?;
                    for (position, node_id) in hops.iter().enumerate() {
                        sqlx::query(
                            "INSERT INTO route_hops(route_id,position,node_id) VALUES(?,?,?)",
                        )
                        .bind(&id)
                        .bind(position as i64)
                        .bind(node_id)
                        .execute(&mut *tx)
                        .await?;
                    }
                    tx.commit().await?;
                    report.routes_updated += 1;
                }
                id
            }
            None => {
                report.routes_created += 1;
                topology::insert_route(
                    pool,
                    &RouteDraft {
                        id: None,
                        label: relay.name.clone(),
                        entry_id: entry_id.clone(),
                        hops,
                        exit_kind: ExitKind::Node,
                        exit_node_id: Some(exit_node_id),
                        exit_landing_id: None,
                    },
                )
                .await?
            }
        };
        route_ids.insert(relay.name.clone(), route_id);
    }

    for desired in &manifest.users {
        let existing = sqlx::query_scalar::<_, String>("SELECT id FROM users WHERE name=?")
            .bind(&desired.name)
            .fetch_optional(pool)
            .await?;
        let user_id = match existing {
            Some(id) => {
                store::users::update_user(
                    pool,
                    &id,
                    Some(desired.quota_bytes),
                    Some(&desired.reset_cycle),
                    Some(desired.expire_at),
                    Some(!desired.enabled),
                )
                .await?;
                report.users_updated += 1;
                id
            }
            None => {
                let (id, token) = store::users::create_user(
                    pool,
                    &desired.name,
                    desired.quota_bytes,
                    &desired.reset_cycle,
                    desired.expire_at,
                )
                .await?;
                report.users_created += 1;
                report.subscription_tokens.push(NewSubscriptionToken {
                    user: desired.name.clone(),
                    token,
                });
                if !desired.enabled {
                    store::users::update_user(pool, &id, None, None, None, Some(true)).await?;
                }
                id
            }
        };

        let desired_routes = desired
            .relays
            .iter()
            .map(|name| route_ids[name].clone())
            .collect::<BTreeSet<_>>();
        let current_rows = store::users::user_routes(pool, &user_id).await?;
        let managed_route_ids = route_ids.values().cloned().collect::<BTreeSet<_>>();
        let current_routes = current_rows
            .iter()
            .map(|r| r.route_id.clone())
            .filter(|id| managed_route_ids.contains(id))
            .collect::<BTreeSet<_>>();
        for route_id in desired_routes.difference(&current_routes) {
            store::users::grant_route(pool, cipher, &user_id, route_id).await?;
            report.grants_created += 1;
        }
        for route_id in &desired_routes {
            store::users::ensure_route_credential(pool, cipher, &user_id, route_id).await?;
        }
        for route_id in current_routes.difference(&desired_routes) {
            let route = topology::get_route(pool, route_id)
                .await?
                .ok_or_else(|| AppError::new(ErrorCode::NotFound, "授权 Route 不存在"))?;
            if route.status == "active" {
                return Err(AppError::new(
                    ErrorCode::Conflict,
                    format!(
                        "用户 {} 撤销 active Route {} 前必须先执行流量结算部署",
                        desired.name, route.label
                    ),
                ));
            }
            store::users::revoke_route(pool, &user_id, route_id).await?;
            report.grants_revoked += 1;
        }
    }

    let _ = store::audit::record(
        pool,
        Some("cli"),
        "manifest.apply",
        Some("manifest"),
        None,
        None,
        Some(&format!(
            "servers={} listeners={} relays={} users={}",
            manifest.servers.len(),
            manifest.listeners.len(),
            manifest.relays.len(),
            manifest.users.len()
        )),
    )
    .await;
    Ok(report)
}

pub fn host_port(host: &str, port: u16) -> String {
    if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cipher() -> Cipher {
        Cipher::from_raw(1, &[7u8; 32]).unwrap()
    }

    fn manifest() -> Manifest {
        toml::from_str(
            r#"
formatVersion=1
includes=[]
nodePort=29736
singboxVersion="1.13.14"

[servers.entry]
ssh="ssh://root@entry.example"
address="entry.example"
[servers.node]
ssh="ssh://root@node.example"
address="node.example"

[protocols.shadowsocks]
method="2022-blake3-aes-128-gcm"
serverKey="auto"
managed=true

[[listeners]]
name="entry-ss"
server="entry"
protocol="shadowsocks"
bind="::"
port=19736

[[relays]]
name="to-node"
listener="entry-ss"
chain=["node"]

[[users]]
name="kimi"
enabled=true
relays=["to-node"]
"#,
        )
        .unwrap()
    }

    fn vless_manifest() -> Manifest {
        toml::from_str(
            r#"
formatVersion=1
includes=[]
nodePort=29736

[servers.entry]
ssh="ssh://root@entry.example"
address="entry.example"
[servers.node]
ssh="ssh://root@node.example"
address="node.example"

[protocols]
[protocols.vless]
flow="xtls-rprx-vision"
privateKey="auto"
shortId="auto"
serverName="itunes.apple.com"
handshakeServer="itunes.apple.com"
handshakePort=443

[[listeners]]
name="entry-vless"
server="entry"
protocol="vless"
bind="::"
port=19736

[[relays]]
name="to-node"
listener="entry-vless"
chain=["node"]

[[users]]
name="bruce"
enabled=true
relays=["to-node"]
"#,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn apply_is_idempotent_and_creates_grant() {
        let path = std::env::temp_dir().join(format!("sbm-apply-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let m = manifest();
        let first = apply(&pool, &cipher(), &m).await.unwrap();
        assert_eq!(first.hosts_created, 2);
        assert_eq!(first.entries_created, 1);
        assert_eq!(first.nodes_created, 1);
        assert_eq!(first.routes_created, 1);
        assert_eq!(first.users_created, 1);
        assert_eq!(first.grants_created, 1);
        assert_eq!(first.subscription_tokens.len(), 1);

        let second = apply(&pool, &cipher(), &m).await.unwrap();
        assert_eq!(second.hosts_created, 0);
        assert_eq!(second.entries_created, 0);
        assert_eq!(second.nodes_created, 0);
        assert_eq!(second.routes_created, 0);
        assert_eq!(second.users_created, 0);
        assert_eq!(second.grants_created, 0);
        assert!(second.subscription_tokens.is_empty());

        let uid: String = sqlx::query_scalar("SELECT id FROM users WHERE name='kimi'")
            .fetch_one(&pool)
            .await
            .unwrap();
        let grants = store::users::user_routes(&pool, &uid).await.unwrap();
        assert_eq!(grants.len(), 1);
        let credential_id: String =
            sqlx::query_scalar("SELECT upsk_credential_id FROM user_routes WHERE user_id=?")
                .bind(&uid)
                .fetch_one(&pool)
                .await
                .unwrap();
        let secret = store::secrets::open_credential(&pool, &cipher(), &credential_id)
            .await
            .unwrap();
        assert!(secret.is_some());
        pool.close().await;
        let _ = std::fs::remove_file(path);
    }

    #[tokio::test]
    async fn vless_grant_uses_encrypted_uuid() {
        let path =
            std::env::temp_dir().join(format!("sbm-apply-vless-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        apply(&pool, &cipher(), &vless_manifest()).await.unwrap();
        let row = sqlx::query(
            "SELECT c.kind, ur.upsk_credential_id
             FROM user_routes ur JOIN credentials c ON c.id=ur.upsk_credential_id",
        )
        .fetch_one(&pool)
        .await
        .unwrap();
        assert_eq!(row.get::<String, _>("kind"), "user_route_uuid");
        let credential_id: String = row.get("upsk_credential_id");
        let uuid = store::secrets::open_credential(&pool, &cipher(), &credential_id)
            .await
            .unwrap()
            .unwrap();
        assert!(uuid::Uuid::parse_str(&uuid).is_ok());
        let entry_id: String = sqlx::query_scalar("SELECT id FROM entries")
            .fetch_one(&pool)
            .await
            .unwrap();
        let first = store::reality::load(&pool, &cipher(), &entry_id)
            .await
            .unwrap()
            .unwrap();
        apply(&pool, &cipher(), &vless_manifest()).await.unwrap();
        let second = store::reality::load(&pool, &cipher(), &entry_id)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            first.secret.private_key.as_str(),
            second.secret.private_key.as_str()
        );
        assert_eq!(
            first.secret.short_id.as_str(),
            second.secret.short_id.as_str()
        );
        pool.close().await;
        let _ = std::fs::remove_file(path);
    }
}

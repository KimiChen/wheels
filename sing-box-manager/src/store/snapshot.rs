//! 从 DB 物化编译输入。拓扑快照（无密钥）与解封密钥（SecretBundle）分两步、分开返回，保编译器纯度。
//! 也提供校验上下文（validate_route 用）。

use std::collections::{HashMap, HashSet};

use sqlx::{Row, SqlitePool};

use crate::compiler::validate::ValidationContext;
use crate::compiler::{EntrySnapshot, RouteSnapshot, Terminal};
use crate::crypto::Cipher;
use crate::domain::host::Capability;
use crate::domain::topology::{ExitKind, LandingKind};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::{secrets, topology};

/// 组装某 Entry 的编译快照（拓扑，无密钥）。
pub async fn load_entry_snapshot(pool: &SqlitePool, entry_id: &str) -> Result<EntrySnapshot> {
    let entry = topology::get_entry(pool, entry_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;

    let route_rows = sqlx::query("SELECT id FROM routes WHERE entry_id=? ORDER BY label")
        .bind(entry_id)
        .fetch_all(pool)
        .await?;

    let mut routes = Vec::new();
    for r in &route_rows {
        let rid: String = r.get("id");
        let route = topology::get_route(pool, &rid).await?.expect("route 存在");
        let hop_recs = topology::route_hops(pool, &rid).await?;
        let mut hops = Vec::new();
        for h in &hop_recs {
            let node = topology::get_node(pool, &h.node_id).await?.ok_or_else(|| {
                AppError::new(
                    ErrorCode::NotFound,
                    format!("hop node 不存在: {}", h.node_id),
                )
            })?;
            hops.push(node);
        }
        let terminal = resolve_terminal(pool, &route).await?;
        routes.push(RouteSnapshot {
            route,
            hops,
            terminal,
        });
    }
    Ok(EntrySnapshot { entry, routes })
}

async fn resolve_terminal(
    pool: &SqlitePool,
    route: &crate::domain::topology::Route,
) -> Result<Terminal> {
    match ExitKind::parse(&route.exit_kind) {
        Some(ExitKind::EntryDirect) => Ok(Terminal::Direct),
        Some(ExitKind::Node) => {
            let nid = route
                .exit_node_id
                .as_deref()
                .ok_or_else(|| AppError::new(ErrorCode::Validation, "node 出口缺 exit_node_id"))?;
            let node = topology::get_node(pool, nid)
                .await?
                .ok_or_else(|| AppError::new(ErrorCode::NotFound, "出口 Node 不存在"))?;
            Ok(Terminal::Node(node))
        }
        Some(ExitKind::Landing) => {
            let lid = route.exit_landing_id.as_deref().ok_or_else(|| {
                AppError::new(ErrorCode::Validation, "landing 出口缺 exit_landing_id")
            })?;
            let landing = topology::get_landing(pool, lid)
                .await?
                .ok_or_else(|| AppError::new(ErrorCode::NotFound, "出口 Landing 不存在"))?;
            match LandingKind::parse(&landing.kind) {
                Some(LandingKind::ManagedNode) => {
                    let nid = landing.node_id.as_deref().ok_or_else(|| {
                        AppError::new(ErrorCode::Validation, "managed_node landing 缺 node_id")
                    })?;
                    let node = topology::get_node(pool, nid).await?.ok_or_else(|| {
                        AppError::new(ErrorCode::NotFound, "landing 引用的 Node 不存在")
                    })?;
                    Ok(Terminal::Node(node))
                }
                Some(LandingKind::Socks5) => Ok(Terminal::Socks5(landing)),
                None => Err(AppError::new(ErrorCode::Validation, "未知 landing 类型")),
            }
        }
        None => Err(AppError::new(ErrorCode::Validation, "未知 exit_kind")),
    }
}

/// 解封某 Entry 快照涉及的全部业务密钥。
pub async fn load_secrets(
    pool: &SqlitePool,
    cipher: &Cipher,
    snap: &EntrySnapshot,
) -> Result<secrets::SecretBundle> {
    let mut bundle = secrets::SecretBundle::default();
    let psk = secrets::open_psk_by_scope(pool, cipher, "entry_psk", &snap.entry.id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "缺 entry_psk"))?;
    bundle.entry_psk.insert(snap.entry.id.clone(), psk);

    for rs in &snap.routes {
        let mut node_ids: Vec<&str> = rs.hops.iter().map(|n| n.id.as_str()).collect();
        if let Terminal::Node(n) = &rs.terminal {
            node_ids.push(&n.id);
        }
        for nid in node_ids {
            if bundle.node_psk.contains_key(nid) {
                continue;
            }
            let p = secrets::open_psk_by_scope(pool, cipher, "node_psk", nid)
                .await?
                .ok_or_else(|| AppError::new(ErrorCode::NotFound, format!("缺 node_psk: {nid}")))?;
            bundle.node_psk.insert(nid.to_string(), p);
        }
        if let Terminal::Socks5(landing) = &rs.terminal {
            if let Some(cred) = &landing.auth_credential_id {
                if let Some(raw) = secrets::open_credential(pool, cipher, cred).await? {
                    bundle
                        .landing_auth
                        .insert(landing.id.clone(), secrets::decode_socks_auth(&raw));
                }
            }
        }
    }
    Ok(bundle)
}

/// 加载 validate_route 所需的既有拓扑上下文（不含密钥）。
pub async fn load_validation_context(pool: &SqlitePool) -> Result<ValidationContext> {
    let entries = topology::list_entries(pool)
        .await?
        .into_iter()
        .map(|e| (e.id.clone(), e))
        .collect();
    let nodes = topology::list_nodes(pool)
        .await?
        .into_iter()
        .map(|n| (n.id.clone(), n))
        .collect();
    let landings = topology::list_landings(pool)
        .await?
        .into_iter()
        .map(|l| (l.id.clone(), l))
        .collect();

    let mut host_caps: HashMap<String, HashSet<Capability>> = HashMap::new();
    let cap_rows = sqlx::query("SELECT host_id, capability FROM host_capabilities")
        .fetch_all(pool)
        .await?;
    for r in &cap_rows {
        if let Some(c) = Capability::parse(&r.get::<String, _>("capability")) {
            host_caps.entry(r.get("host_id")).or_default().insert(c);
        }
    }
    Ok(ValidationContext {
        entries,
        nodes,
        landings,
        host_caps,
    })
}

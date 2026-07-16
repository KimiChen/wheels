//! entries / nodes / landings / routes / route_hops 仓储。创建即信封封存 SS-2022 PSK；
//! 删除守卫返回可读 Conflict（而非裸 SQLITE_CONSTRAINT）；对象删除时同事务清理其凭据。

use sqlx::{Row, SqlitePool};

use crate::compiler::psk::{generate_psk, NODE_SS_METHOD};
use crate::crypto::Cipher;
use crate::domain::topology::{
    Entry, Landing, LandingKind, Network, Node, Route, RouteDraft, RouteHop, ENTRY_PORT, NODE_PORT,
};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::now_unix;
use crate::store::secrets;

// ---------- Entry ----------

pub struct NewEntry<'a> {
    pub host_id: &'a str,
    pub public_address: &'a str,
    pub inbound_kind: InboundKindArg,
    pub ss_method: Option<&'a str>,
    pub allow_direct: bool,
}

pub type InboundKindArg = crate::domain::topology::InboundKind;

/// 创建 Entry 并生成/封存 entry_psk（单事务）。
pub async fn create_entry(pool: &SqlitePool, cipher: &Cipher, ne: &NewEntry<'_>) -> Result<String> {
    // R9（平台内）：一个 Host 至多一个 Entry（独占 19736）。
    let existing: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM entries WHERE host_id=?")
        .bind(ne.host_id)
        .fetch_one(pool)
        .await?;
    if existing > 0 {
        return Err(AppError::new(
            ErrorCode::Conflict,
            format!("Host {} 已有 Entry（19736 独占）", ne.host_id),
        ));
    }
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let method = ne.ss_method.unwrap_or(NODE_SS_METHOD);
    let psk = generate_psk(method);
    let mut tx = pool.begin().await?;
    sqlx::query(
        "INSERT INTO entries(id,host_id,public_address,port,inbound_kind,ss_method,allow_direct,created_at,updated_at)
         VALUES(?,?,?,?,?,?,?,?,?)",
    )
    .bind(&id)
    .bind(ne.host_id)
    .bind(ne.public_address)
    .bind(ENTRY_PORT)
    .bind(ne.inbound_kind.as_str())
    .bind(ne.ss_method)
    .bind(ne.allow_direct as i64)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    secrets::put_psk_tx(&mut tx, cipher, "entry_psk", &id, &psk).await?;
    tx.commit().await?;
    Ok(id)
}

pub async fn get_entry(pool: &SqlitePool, id: &str) -> Result<Option<Entry>> {
    let row = sqlx::query(
        "SELECT id,host_id,public_address,port,inbound_kind,ss_method,allow_direct,current_revision FROM entries WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_entry))
}

pub async fn list_entries(pool: &SqlitePool) -> Result<Vec<Entry>> {
    let rows = sqlx::query(
        "SELECT id,host_id,public_address,port,inbound_kind,ss_method,allow_direct,current_revision FROM entries ORDER BY id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_entry).collect())
}

/// 删除 Entry：级联其 routes/route_hops（0001 FK CASCADE），同事务删 entry_psk 凭据。
pub async fn delete_entry(pool: &SqlitePool, id: &str) -> Result<()> {
    let mut tx = pool.begin().await?;
    secrets::delete_psk_tx(&mut tx, "entry_psk", id).await?;
    let res = sqlx::query("DELETE FROM entries WHERE id=?")
        .bind(id)
        .execute(&mut *tx)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(ErrorCode::NotFound, "entry 不存在"));
    }
    tx.commit().await?;
    Ok(())
}

fn row_to_entry(r: &sqlx::sqlite::SqliteRow) -> Entry {
    Entry {
        id: r.get("id"),
        host_id: r.get("host_id"),
        public_address: r.get("public_address"),
        port: r.get("port"),
        inbound_kind: r.get("inbound_kind"),
        ss_method: r.get("ss_method"),
        allow_direct: r.get::<i64, _>("allow_direct") != 0,
        current_revision: r.get("current_revision"),
    }
}

// ---------- Node ----------

/// 创建 Node 并生成/封存 node_psk（固定 NODE_SS_METHOD）。
pub async fn create_node(
    pool: &SqlitePool,
    cipher: &Cipher,
    host_id: &str,
    data_address: &str,
    allow_direct_exit: bool,
) -> Result<String> {
    // R9（平台内）：一个 Host 至多一个 Node（独占 29736）。
    let existing: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM nodes WHERE host_id=?")
        .bind(host_id)
        .fetch_one(pool)
        .await?;
    if existing > 0 {
        return Err(AppError::new(
            ErrorCode::Conflict,
            format!("Host {host_id} 已有 Node（29736 独占）"),
        ));
    }
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let psk = generate_psk(NODE_SS_METHOD);
    let mut tx = pool.begin().await?;
    sqlx::query(
        "INSERT INTO nodes(id,host_id,data_address,port,allow_direct_exit,created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
    )
    .bind(&id)
    .bind(host_id)
    .bind(data_address)
    .bind(NODE_PORT)
    .bind(allow_direct_exit as i64)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    secrets::put_psk_tx(&mut tx, cipher, "node_psk", &id, &psk).await?;
    tx.commit().await?;
    Ok(id)
}

pub async fn get_node(pool: &SqlitePool, id: &str) -> Result<Option<Node>> {
    let row = sqlx::query(
        "SELECT id,host_id,data_address,port,allow_direct_exit,current_revision FROM nodes WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_node))
}

pub async fn list_nodes(pool: &SqlitePool) -> Result<Vec<Node>> {
    let rows = sqlx::query(
        "SELECT id,host_id,data_address,port,allow_direct_exit,current_revision FROM nodes ORDER BY id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_node).collect())
}

/// 删除 Node：先查引用（route_hops.node_id ∪ routes.exit_node_id ∪ landings.node_id）；
/// 命中即 Conflict 列出引用者；否则同事务删 node_psk + node。
pub async fn delete_node(pool: &SqlitePool, id: &str) -> Result<()> {
    let mut refs: Vec<String> = Vec::new();
    let hop_labels = sqlx::query(
        "SELECT DISTINCT r.label FROM route_hops rh JOIN routes r ON r.id=rh.route_id WHERE rh.node_id=? ORDER BY r.label",
    )
    .bind(id)
    .fetch_all(pool)
    .await?;
    refs.extend(
        hop_labels
            .iter()
            .map(|r| format!("route:{}", r.get::<String, _>("label"))),
    );
    let exit_labels = sqlx::query("SELECT label FROM routes WHERE exit_node_id=? ORDER BY label")
        .bind(id)
        .fetch_all(pool)
        .await?;
    refs.extend(
        exit_labels
            .iter()
            .map(|r| format!("route-exit:{}", r.get::<String, _>("label"))),
    );
    let landing_ids = sqlx::query("SELECT id FROM landings WHERE node_id=? ORDER BY id")
        .bind(id)
        .fetch_all(pool)
        .await?;
    refs.extend(
        landing_ids
            .iter()
            .map(|r| format!("landing:{}", r.get::<String, _>("id"))),
    );
    if !refs.is_empty() {
        return Err(AppError::new(
            ErrorCode::Conflict,
            format!("Node 仍被引用，不能删除：{}", refs.join(", ")),
        ));
    }
    let mut tx = pool.begin().await?;
    secrets::delete_psk_tx(&mut tx, "node_psk", id).await?;
    let res = sqlx::query("DELETE FROM nodes WHERE id=?")
        .bind(id)
        .execute(&mut *tx)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(ErrorCode::NotFound, "node 不存在"));
    }
    tx.commit().await?;
    Ok(())
}

fn row_to_node(r: &sqlx::sqlite::SqliteRow) -> Node {
    Node {
        id: r.get("id"),
        host_id: r.get("host_id"),
        data_address: r.get("data_address"),
        port: r.get("port"),
        allow_direct_exit: r.get::<i64, _>("allow_direct_exit") != 0,
        current_revision: r.get("current_revision"),
    }
}

// ---------- Landing ----------

pub struct NewLanding<'a> {
    pub kind: LandingKind,
    pub node_id: Option<&'a str>,
    pub socks5_address: Option<&'a str>,
    pub socks5_port: Option<i64>,
    pub network: Network,
    pub socks_user: Option<&'a str>,
    pub socks_pass: Option<&'a str>,
}

/// 创建 Landing。socks5 且带认证时封存 landing_auth 并回填 auth_credential_id（同事务）。
pub async fn create_landing(
    pool: &SqlitePool,
    cipher: &Cipher,
    nl: &NewLanding<'_>,
) -> Result<String> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let mut tx = pool.begin().await?;
    let auth_cred_id = match (nl.kind, nl.socks_user, nl.socks_pass) {
        (LandingKind::Socks5, Some(u), Some(p)) => Some(
            secrets::put_psk_tx(
                &mut tx,
                cipher,
                "landing_auth",
                &id,
                &secrets::encode_socks_auth(u, p),
            )
            .await?,
        ),
        _ => None,
    };
    sqlx::query(
        "INSERT INTO landings(id,kind,node_id,socks5_address,socks5_port,network,auth_credential_id,created_at,updated_at)
         VALUES(?,?,?,?,?,?,?,?,?)",
    )
    .bind(&id)
    .bind(nl.kind.as_str())
    .bind(nl.node_id)
    .bind(nl.socks5_address)
    .bind(nl.socks5_port)
    .bind(nl.network.as_str())
    .bind(&auth_cred_id)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(id)
}

pub async fn get_landing(pool: &SqlitePool, id: &str) -> Result<Option<Landing>> {
    let row = sqlx::query(
        "SELECT id,kind,node_id,socks5_address,socks5_port,network,auth_credential_id FROM landings WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_landing))
}

pub async fn list_landings(pool: &SqlitePool) -> Result<Vec<Landing>> {
    let rows = sqlx::query(
        "SELECT id,kind,node_id,socks5_address,socks5_port,network,auth_credential_id FROM landings ORDER BY id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_landing).collect())
}

/// 删除 Landing：被 routes.exit_landing_id 引用则 Conflict；否则同事务删 landing_auth + landing。
pub async fn delete_landing(pool: &SqlitePool, id: &str) -> Result<()> {
    let refs = sqlx::query("SELECT label FROM routes WHERE exit_landing_id=? ORDER BY label")
        .bind(id)
        .fetch_all(pool)
        .await?;
    if !refs.is_empty() {
        let labels: Vec<String> = refs.iter().map(|r| r.get::<String, _>("label")).collect();
        return Err(AppError::new(
            ErrorCode::Conflict,
            format!("Landing 仍被 Route 引用，不能删除：{}", labels.join(", ")),
        ));
    }
    let landing = get_landing(pool, id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "landing 不存在"))?;
    let mut tx = pool.begin().await?;
    if let Some(cred) = &landing.auth_credential_id {
        sqlx::query("DELETE FROM credentials WHERE id=?")
            .bind(cred)
            .execute(&mut *tx)
            .await?;
    }
    sqlx::query("DELETE FROM landings WHERE id=?")
        .bind(id)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;
    Ok(())
}

fn row_to_landing(r: &sqlx::sqlite::SqliteRow) -> Landing {
    Landing {
        id: r.get("id"),
        kind: r.get("kind"),
        node_id: r.get("node_id"),
        socks5_address: r.get("socks5_address"),
        socks5_port: r.get("socks5_port"),
        network: r.get("network"),
        auth_credential_id: r.get("auth_credential_id"),
    }
}

// ---------- Route ----------

/// 插入一条 Route（status='draft'）及其有序 hops（单事务）。校验在上层先行。
pub async fn insert_route(pool: &SqlitePool, draft: &RouteDraft) -> Result<String> {
    let id = draft
        .id
        .clone()
        .unwrap_or_else(|| uuid::Uuid::new_v4().to_string());
    let now = now_unix();
    let mut tx = pool.begin().await?;
    sqlx::query(
        "INSERT INTO routes(id,label,entry_id,exit_kind,exit_node_id,exit_landing_id,status,created_at,updated_at)
         VALUES(?,?,?,?,?,?, 'draft',?,?)",
    )
    .bind(&id)
    .bind(&draft.label)
    .bind(&draft.entry_id)
    .bind(draft.exit_kind.as_str())
    .bind(&draft.exit_node_id)
    .bind(&draft.exit_landing_id)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    for (pos, node_id) in draft.hops.iter().enumerate() {
        sqlx::query("INSERT INTO route_hops(route_id,position,node_id) VALUES(?,?,?)")
            .bind(&id)
            .bind(pos as i64)
            .bind(node_id)
            .execute(&mut *tx)
            .await?;
    }
    tx.commit().await?;
    Ok(id)
}

pub async fn delete_route(pool: &SqlitePool, id: &str) -> Result<()> {
    let res = sqlx::query("DELETE FROM routes WHERE id=?")
        .bind(id)
        .execute(pool)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(ErrorCode::NotFound, "route 不存在"));
    }
    Ok(())
}

pub async fn get_route(pool: &SqlitePool, id: &str) -> Result<Option<Route>> {
    let row = sqlx::query(
        "SELECT id,label,entry_id,exit_kind,exit_node_id,exit_landing_id,status FROM routes WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_route))
}

pub async fn list_routes(pool: &SqlitePool) -> Result<Vec<Route>> {
    let rows = sqlx::query(
        "SELECT id,label,entry_id,exit_kind,exit_node_id,exit_landing_id,status FROM routes ORDER BY label",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_route).collect())
}

pub async fn route_hops(pool: &SqlitePool, route_id: &str) -> Result<Vec<RouteHop>> {
    let rows =
        sqlx::query("SELECT position,node_id FROM route_hops WHERE route_id=? ORDER BY position")
            .bind(route_id)
            .fetch_all(pool)
            .await?;
    Ok(rows
        .iter()
        .map(|r| RouteHop {
            position: r.get("position"),
            node_id: r.get("node_id"),
        })
        .collect())
}

/// label 是否已被别的 Route 占用（排除 `exclude_id`）。
pub async fn label_taken(pool: &SqlitePool, label: &str, exclude_id: Option<&str>) -> Result<bool> {
    let n: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM routes WHERE label=? AND id<>?")
        .bind(label)
        .bind(exclude_id.unwrap_or(""))
        .fetch_one(pool)
        .await?;
    Ok(n > 0)
}

fn row_to_route(r: &sqlx::sqlite::SqliteRow) -> Route {
    Route {
        id: r.get("id"),
        label: r.get("label"),
        entry_id: r.get("entry_id"),
        exit_kind: r.get("exit_kind"),
        exit_node_id: r.get("exit_node_id"),
        exit_landing_id: r.get("exit_landing_id"),
        status: r.get("status"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::ExitKind;
    use crate::store;
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }
    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-topo-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }

    #[tokio::test]
    async fn create_entry_node_generate_psk_and_delete_guard() {
        let pool = pool().await;
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh = store::hosts::create_host(&pool, "nh", None, &[Capability::Node])
            .await
            .unwrap();
        let entry = create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e.example.com",
                inbound_kind: InboundKindArg::Shadowsocks,
                ss_method: None,
                allow_direct: true,
            },
        )
        .await
        .unwrap();
        let node = create_node(&pool, &c, &nh, "n.example.com", true)
            .await
            .unwrap();

        // PSK 已封存且可解封，长度匹配（默认 16B → base64）。
        let epsk = secrets::open_psk_by_scope(&pool, &c, "entry_psk", &entry)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(STANDARD.decode(&epsk).unwrap().len(), 16);
        assert!(secrets::open_psk_by_scope(&pool, &c, "node_psk", &node)
            .await
            .unwrap()
            .is_some());

        // 建一条引用该 node 的 Route，则删 node 被拒。
        let draft = RouteDraft {
            id: None,
            label: "r1".into(),
            entry_id: entry.clone(),
            hops: vec![node.clone()],
            exit_kind: ExitKind::Node,
            exit_node_id: Some(node.clone()),
            exit_landing_id: None,
        };
        insert_route(&pool, &draft).await.unwrap();
        let err = delete_node(&pool, &node).await.unwrap_err();
        assert_eq!(err.code, ErrorCode::Conflict);

        // 删 Route 后可删 node，且 node_psk 一并清理。
        let rid = get_route_by_label(&pool, "r1").await;
        delete_route(&pool, &rid).await.unwrap();
        delete_node(&pool, &node).await.unwrap();
        assert!(secrets::open_psk_by_scope(&pool, &c, "node_psk", &node)
            .await
            .unwrap()
            .is_none());
        pool.close().await;
    }

    async fn get_route_by_label(pool: &SqlitePool, label: &str) -> String {
        sqlx::query_scalar::<_, String>("SELECT id FROM routes WHERE label=?")
            .bind(label)
            .fetch_one(pool)
            .await
            .unwrap()
    }
}

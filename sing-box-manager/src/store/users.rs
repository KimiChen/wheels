//! 用户 CRUD + Route ACL（grant/revoke，事务生成身份+uPSK）+ 订阅 token + 编译/reconcile 两投影。
//! 身份名由不可变 (user_id,route_id) 确定性派生；uPSK 信封存 credentials(kind=user_route_upsk)。

use std::collections::BTreeMap;

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use sqlx::{Row, SqlitePool};

use crate::compiler::psk::{generate_psk, NODE_SS_METHOD};
use crate::crypto::Cipher;
use crate::domain::user::{User, UserRouteRow};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::{now_unix, secrets};

/// 由不可变 (user_id, route_id) 派生的稳定身份名。仅 [a-z0-9]，字母前缀（SSM username 合法，非纯数字，
/// 不含 ':' 避免与 EIH 密码分隔符冲突）。改用户/Route 名不影响身份。
pub fn identity_name(user_id: &str, route_id: &str) -> String {
    let mut input = Vec::with_capacity(user_id.len() + route_id.len() + 1);
    input.extend_from_slice(user_id.as_bytes());
    input.push(0x1f);
    input.extend_from_slice(route_id.as_bytes());
    format!("u{}", &crate::pki::sha256_hex(&input)[..24])
}

fn sanitize(s: &str) -> String {
    let out: String = s
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
        .take(40)
        .collect();
    if out.is_empty() {
        "x".into()
    } else {
        out
    }
}

/// 生成一个高强度订阅 token（32B CSPRNG base64url）及其 sha256 hash。
fn generate_token() -> (String, String) {
    use rand::RngCore;
    let mut buf = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut buf);
    let plaintext = URL_SAFE_NO_PAD.encode(buf);
    let hash = crate::pki::sha256_hex(plaintext.as_bytes());
    (plaintext, hash)
}

fn row_to_user(r: &sqlx::sqlite::SqliteRow) -> User {
    User {
        id: r.get("id"),
        name: r.get("name"),
        quota_bytes: r.get("quota_bytes"),
        reset_cycle: r.get("reset_cycle"),
        expire_at: r.get("expire_at"),
        disabled: r.get::<i64, _>("disabled") != 0,
        created_at: r.get("created_at"),
        updated_at: r.get("updated_at"),
    }
}

const USER_COLS: &str = "id,name,quota_bytes,reset_cycle,expire_at,disabled,created_at,updated_at";

/// 创建用户并同事务铸造订阅 token（明文一次性返回）。
pub async fn create_user(
    pool: &SqlitePool,
    name: &str,
    quota_bytes: i64,
    reset_cycle: &str,
    expire_at: Option<i64>,
) -> Result<(String, String)> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let (token, hash) = generate_token();
    let mut tx = pool.begin().await?;
    sqlx::query(
        "INSERT INTO users(id,name,quota_bytes,reset_cycle,expire_at,disabled,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?)",
    )
    .bind(&id)
    .bind(name)
    .bind(quota_bytes)
    .bind(reset_cycle)
    .bind(expire_at)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    sqlx::query("INSERT INTO subscription_tokens(user_id,token_hash,created_at) VALUES(?,?,?)")
        .bind(&id)
        .bind(&hash)
        .bind(now)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;
    Ok((id, token))
}

pub async fn get_user(pool: &SqlitePool, id: &str) -> Result<Option<User>> {
    let row = sqlx::query(&format!("SELECT {USER_COLS} FROM users WHERE id=?"))
        .bind(id)
        .fetch_optional(pool)
        .await?;
    Ok(row.as_ref().map(row_to_user))
}

pub async fn list_users(pool: &SqlitePool) -> Result<Vec<User>> {
    let rows = sqlx::query(&format!("SELECT {USER_COLS} FROM users ORDER BY name"))
        .fetch_all(pool)
        .await?;
    Ok(rows.iter().map(row_to_user).collect())
}

/// 改配额/周期/有效期/停用（部分更新，None 保持不变）。
pub async fn update_user(
    pool: &SqlitePool,
    id: &str,
    quota_bytes: Option<i64>,
    reset_cycle: Option<&str>,
    expire_at: Option<Option<i64>>,
    disabled: Option<bool>,
) -> Result<()> {
    let now = now_unix();
    let mut tx = pool.begin().await?;
    if let Some(q) = quota_bytes {
        sqlx::query("UPDATE users SET quota_bytes=?, updated_at=? WHERE id=?")
            .bind(q)
            .bind(now)
            .bind(id)
            .execute(&mut *tx)
            .await?;
    }
    if let Some(c) = reset_cycle {
        sqlx::query("UPDATE users SET reset_cycle=?, updated_at=? WHERE id=?")
            .bind(c)
            .bind(now)
            .bind(id)
            .execute(&mut *tx)
            .await?;
    }
    if let Some(e) = expire_at {
        sqlx::query("UPDATE users SET expire_at=?, updated_at=? WHERE id=?")
            .bind(e)
            .bind(now)
            .bind(id)
            .execute(&mut *tx)
            .await?;
    }
    if let Some(d) = disabled {
        sqlx::query("UPDATE users SET disabled=?, updated_at=? WHERE id=?")
            .bind(d as i64)
            .bind(now)
            .bind(id)
            .execute(&mut *tx)
            .await?;
    }
    tx.commit().await?;
    Ok(())
}

/// 删用户：0001 FK CASCADE 连带 user_routes/subscription_tokens；先 sweep 清各 grant 的 uPSK 凭据。
pub async fn delete_user(pool: &SqlitePool, id: &str) -> Result<()> {
    let names = sqlx::query(
        "SELECT identity_name FROM user_routes WHERE user_id=? AND identity_name IS NOT NULL",
    )
    .bind(id)
    .fetch_all(pool)
    .await?;
    let mut tx = pool.begin().await?;
    for n in &names {
        secrets::delete_psk_tx(
            &mut tx,
            "user_route_upsk",
            &n.get::<String, _>("identity_name"),
        )
        .await?;
    }
    let res = sqlx::query("DELETE FROM users WHERE id=?")
        .bind(id)
        .execute(&mut *tx)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(ErrorCode::NotFound, "user 不存在"));
    }
    tx.commit().await?;
    Ok(())
}

/// 授权一条 Route：生成 identity + uPSK（长度按目标 Entry.ss_method），信封封存，写 user_routes。返回 identity_name。
pub async fn grant_route(
    pool: &SqlitePool,
    cipher: &Cipher,
    user_id: &str,
    route_id: &str,
) -> Result<String> {
    let user = get_user(pool, user_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "user 不存在"))?;
    let route = crate::store::topology::get_route(pool, route_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "route 不存在"))?;
    let entry = crate::store::topology::get_entry(pool, &route.entry_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;
    let exists: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM user_routes WHERE user_id=? AND route_id=?")
            .bind(user_id)
            .bind(route_id)
            .fetch_one(pool)
            .await?;
    if exists > 0 {
        return Err(AppError::new(ErrorCode::Conflict, "已授权该 Route"));
    }
    let name = identity_name(user_id, route_id);
    let label = format!("{}-{}", sanitize(&user.name), sanitize(&route.label));
    let method = entry.ss_method.as_deref().unwrap_or(NODE_SS_METHOD);
    let upsk = generate_psk(method);
    let now = now_unix();
    let mut tx = pool.begin().await?;
    let cid = secrets::put_psk_tx(&mut tx, cipher, "user_route_upsk", &name, &upsk).await?;
    sqlx::query(
        "INSERT INTO user_routes(user_id,route_id,identity_name,identity_label,upsk_credential_id,created_at) VALUES(?,?,?,?,?,?)",
    )
    .bind(user_id)
    .bind(route_id)
    .bind(&name)
    .bind(&label)
    .bind(&cid)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(name)
}

/// 撤销授权：删 user_routes 行 + 清其 uPSK 凭据（同事务）。
pub async fn revoke_route(pool: &SqlitePool, user_id: &str, route_id: &str) -> Result<()> {
    let name = identity_name(user_id, route_id);
    let mut tx = pool.begin().await?;
    secrets::delete_psk_tx(&mut tx, "user_route_upsk", &name).await?;
    let res = sqlx::query("DELETE FROM user_routes WHERE user_id=? AND route_id=?")
        .bind(user_id)
        .bind(route_id)
        .execute(&mut *tx)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(ErrorCode::NotFound, "未授权该 Route"));
    }
    tx.commit().await?;
    Ok(())
}

/// 轮换订阅 token（旧 hash 立即失效），明文一次性返回。
pub async fn rotate_token(pool: &SqlitePool, user_id: &str) -> Result<String> {
    let (token, hash) = generate_token();
    let now = now_unix();
    let res =
        sqlx::query("UPDATE subscription_tokens SET token_hash=?, created_at=? WHERE user_id=?")
            .bind(&hash)
            .bind(now)
            .bind(user_id)
            .execute(pool)
            .await?;
    if res.rows_affected() == 0 {
        sqlx::query("INSERT INTO subscription_tokens(user_id,token_hash,created_at) VALUES(?,?,?)")
            .bind(user_id)
            .bind(&hash)
            .bind(now)
            .execute(pool)
            .await?;
    }
    Ok(token)
}

pub async fn lookup_user_by_token_hash(
    pool: &SqlitePool,
    token_hash: &str,
) -> Result<Option<User>> {
    let row = sqlx::query(&format!(
        "SELECT {} FROM users u JOIN subscription_tokens st ON st.user_id=u.id WHERE st.token_hash=?",
        USER_COLS.split(',').map(|c| format!("u.{c}")).collect::<Vec<_>>().join(",")
    ))
    .bind(token_hash)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_user))
}

pub async fn user_routes(pool: &SqlitePool, user_id: &str) -> Result<Vec<UserRouteRow>> {
    let rows = sqlx::query(
        "SELECT ur.route_id, r.label, r.status, ur.identity_name, ur.identity_label
         FROM user_routes ur JOIN routes r ON r.id=ur.route_id WHERE ur.user_id=? ORDER BY r.label",
    )
    .bind(user_id)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| UserRouteRow {
            route_id: r.get("route_id"),
            route_label: r.get("label"),
            route_status: r.get("status"),
            identity_name: r.get("identity_name"),
            identity_label: r.get("identity_label"),
        })
        .collect())
}

/// 编译投影：某 Entry 全部 draft|active Route 上的全部已授权身份名（结构态，与 disabled/expire 无关）。
pub async fn configured_identities(
    pool: &SqlitePool,
    entry_id: &str,
) -> Result<BTreeMap<String, Vec<String>>> {
    let rows = sqlx::query(
        "SELECT ur.route_id, ur.identity_name FROM user_routes ur JOIN routes r ON r.id=ur.route_id
         WHERE r.entry_id=? AND ur.identity_name IS NOT NULL AND r.status IN ('draft','active')
         ORDER BY ur.route_id, ur.identity_name",
    )
    .bind(entry_id)
    .fetch_all(pool)
    .await?;
    let mut map: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for r in &rows {
        map.entry(r.get("route_id"))
            .or_default()
            .push(r.get("identity_name"));
    }
    Ok(map)
}

/// reconcile 投影：某 Entry 的期望 SSM 身份集（identity_name → uPSK），仅 active Route + 资格用户。
pub async fn eligible_desired(
    pool: &SqlitePool,
    cipher: &Cipher,
    entry_id: &str,
    now: i64,
) -> Result<BTreeMap<String, String>> {
    // 资格 = ACL 授权 ∧ route active ∧ 未停用 ∧ 未过期 ∧ 未超额（user_runtime_state.effective_disabled，Phase 5）。
    let rows = sqlx::query(
        "SELECT ur.identity_name, ur.upsk_credential_id FROM user_routes ur
         JOIN routes r ON r.id=ur.route_id JOIN users u ON u.id=ur.user_id
         LEFT JOIN user_runtime_state urs ON urs.user_id=u.id
         WHERE r.entry_id=? AND ur.identity_name IS NOT NULL AND r.status='active'
           AND u.disabled=0 AND (u.expire_at IS NULL OR u.expire_at>?)
           AND COALESCE(urs.effective_disabled,0)=0
         ORDER BY ur.identity_name",
    )
    .bind(entry_id)
    .bind(now)
    .fetch_all(pool)
    .await?;
    let mut map = BTreeMap::new();
    for r in &rows {
        let name: String = r.get("identity_name");
        let cid: Option<String> = r.get("upsk_credential_id");
        if let Some(cid) = cid {
            if let Some(upsk) = secrets::open_credential(pool, cipher, &cid).await? {
                map.insert(name, upsk);
            }
        }
    }
    Ok(map)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::store::topology::NewEntry;
    use crate::store::{self, topology as topo};
    use base64::engine::general_purpose::STANDARD;

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }
    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-users-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }

    #[test]
    fn identity_name_deterministic_and_legal() {
        let a = identity_name("user-1", "route-1");
        assert_eq!(a, identity_name("user-1", "route-1")); // 稳定
        assert_ne!(a, identity_name("user-1", "route-2"));
        assert_ne!(a, identity_name("user-2", "route-1"));
        assert!(a.starts_with('u') && a.len() == 25);
        assert!(a.chars().all(|c| c.is_ascii_alphanumeric()) && !a.contains(':'));
    }

    #[tokio::test]
    async fn crud_grant_revoke_and_projections() {
        let pool = pool().await;
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh = store::hosts::create_host(&pool, "nh", None, &[Capability::Node])
            .await
            .unwrap();
        let e1 = topo::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e.example.com",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let n1 = topo::create_node(&pool, &c, &nh, "n1.example.com", true)
            .await
            .unwrap();
        let r1 = topo::insert_route(
            &pool,
            &RouteDraft {
                id: None,
                label: "r1".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(n1),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();

        let (uid, token) = create_user(&pool, "alice", 10 << 30, "monthly", None)
            .await
            .unwrap();
        assert!(!token.is_empty());
        // token 查找。
        let hash = crate::pki::sha256_hex(token.as_bytes());
        assert_eq!(
            lookup_user_by_token_hash(&pool, &hash)
                .await
                .unwrap()
                .unwrap()
                .name,
            "alice"
        );

        let name = grant_route(&pool, &c, &uid, &r1).await.unwrap();
        assert_eq!(name, identity_name(&uid, &r1));
        // uPSK 已封存。
        assert!(
            secrets::open_psk_by_scope(&pool, &c, "user_route_upsk", &name)
                .await
                .unwrap()
                .is_some()
        );
        // configured 含该身份（route draft）。
        let conf = configured_identities(&pool, &e1).await.unwrap();
        assert_eq!(conf.get(&r1).unwrap(), &vec![name.clone()]);
        // eligible 为空（route 未 active）。
        assert!(eligible_desired(&pool, &c, &e1, now_unix())
            .await
            .unwrap()
            .is_empty());
        // 激活 route 后 eligible 含该身份。
        sqlx::query("UPDATE routes SET status='active' WHERE id=?")
            .bind(&r1)
            .execute(&pool)
            .await
            .unwrap();
        let elig = eligible_desired(&pool, &c, &e1, now_unix()).await.unwrap();
        assert!(elig.contains_key(&name));
        // 停用用户 → eligible 空，但 configured 仍含（结构态）。
        update_user(&pool, &uid, None, None, None, Some(true))
            .await
            .unwrap();
        assert!(eligible_desired(&pool, &c, &e1, now_unix())
            .await
            .unwrap()
            .is_empty());
        assert!(!configured_identities(&pool, &e1).await.unwrap().is_empty());

        // revoke → 凭据清理 + user_routes 删除。
        revoke_route(&pool, &uid, &r1).await.unwrap();
        assert!(
            secrets::open_psk_by_scope(&pool, &c, "user_route_upsk", &name)
                .await
                .unwrap()
                .is_none()
        );
        assert!(configured_identities(&pool, &e1).await.unwrap().is_empty());
        pool.close().await;
    }

    #[tokio::test]
    async fn rotate_token_invalidates_old() {
        let pool = pool().await;
        let (uid, t1) = create_user(&pool, "bob", 0, "never", None).await.unwrap();
        let t2 = rotate_token(&pool, &uid).await.unwrap();
        assert_ne!(t1, t2);
        assert!(
            lookup_user_by_token_hash(&pool, &crate::pki::sha256_hex(t1.as_bytes()))
                .await
                .unwrap()
                .is_none()
        );
        assert!(
            lookup_user_by_token_hash(&pool, &crate::pki::sha256_hex(t2.as_bytes()))
                .await
                .unwrap()
                .is_some()
        );
        pool.close().await;
    }
}

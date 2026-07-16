//! admin_sessions 仓储：会话与 CSRF token 只存 sha256（明文只在 Cookie / 登录响应体）。
//! 双过期：idle（滑动续期）+ absolute（硬顶）。改密/禁用后按 admin 批量吊销。

use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

#[derive(Debug, Clone)]
pub struct Session {
    pub id_hash: String,
    pub admin_id: String,
    pub csrf_hash: String,
    pub created_at: i64,
    pub last_seen_at: i64,
    pub last_reauth_at: Option<i64>,
    pub idle_expires_at: i64,
    pub absolute_expires_at: i64,
}

/// 32B CSPRNG → base64url，返回 (明文, sha256)。明文仅此一次出现。
fn gen_secret() -> (String, String) {
    use base64::engine::general_purpose::URL_SAFE_NO_PAD;
    use base64::Engine;
    use rand::RngCore;
    let mut buf = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut buf);
    let plaintext = URL_SAFE_NO_PAD.encode(buf);
    let hash = crate::pki::sha256_hex(plaintext.as_bytes());
    (plaintext, hash)
}

/// 建会话。返回 (明文 session token, 明文 csrf token)；入库只存两者 sha256。
pub async fn create(
    pool: &SqlitePool,
    admin_id: &str,
    idle_ttl: i64,
    abs_ttl: i64,
    ip: Option<&str>,
    ua: Option<&str>,
) -> Result<(String, String)> {
    let (sid, sid_hash) = gen_secret();
    let (csrf, csrf_hash) = gen_secret();
    let now = now_unix();
    sqlx::query(
        "INSERT INTO admin_sessions(id_hash,admin_id,csrf_hash,created_at,last_seen_at,idle_expires_at,absolute_expires_at,ip,user_agent)
         VALUES(?,?,?,?,?,?,?,?,?)",
    )
    .bind(&sid_hash)
    .bind(admin_id)
    .bind(&csrf_hash)
    .bind(now)
    .bind(now)
    .bind(now + idle_ttl)
    .bind(now + abs_ttl)
    .bind(ip)
    .bind(ua)
    .execute(pool)
    .await?;
    Ok((sid, csrf))
}

/// 按 id_hash 取仍有效的会话（双过期校验；过期即视为无）。
pub async fn lookup_valid(pool: &SqlitePool, id_hash: &str, now: i64) -> Result<Option<Session>> {
    let row = sqlx::query(
        "SELECT * FROM admin_sessions WHERE id_hash=? AND idle_expires_at>? AND absolute_expires_at>?",
    )
    .bind(id_hash)
    .bind(now)
    .bind(now)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| Session {
        id_hash: r.get("id_hash"),
        admin_id: r.get("admin_id"),
        csrf_hash: r.get("csrf_hash"),
        created_at: r.get("created_at"),
        last_seen_at: r.get("last_seen_at"),
        last_reauth_at: r.get("last_reauth_at"),
        idle_expires_at: r.get("idle_expires_at"),
        absolute_expires_at: r.get("absolute_expires_at"),
    }))
}

/// 滑动续期 idle（不动 absolute 硬顶）。
pub async fn touch(pool: &SqlitePool, id_hash: &str, now: i64, idle_ttl: i64) -> Result<()> {
    sqlx::query("UPDATE admin_sessions SET last_seen_at=?, idle_expires_at=? WHERE id_hash=?")
        .bind(now)
        .bind(now + idle_ttl)
        .bind(id_hash)
        .execute(pool)
        .await?;
    Ok(())
}

/// 标记一次 re-auth。
pub async fn stamp_reauth(pool: &SqlitePool, id_hash: &str, now: i64) -> Result<()> {
    sqlx::query("UPDATE admin_sessions SET last_reauth_at=? WHERE id_hash=?")
        .bind(now)
        .bind(id_hash)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn revoke(pool: &SqlitePool, id_hash: &str) -> Result<()> {
    sqlx::query("DELETE FROM admin_sessions WHERE id_hash=?")
        .bind(id_hash)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn revoke_all_for_admin(pool: &SqlitePool, admin_id: &str) -> Result<()> {
    sqlx::query("DELETE FROM admin_sessions WHERE admin_id=?")
        .bind(admin_id)
        .execute(pool)
        .await?;
    Ok(())
}

/// 后台 GC：删除双过期任一到期的会话。返回删除条数。
pub async fn gc_expired(pool: &SqlitePool, now: i64) -> Result<u64> {
    let r = sqlx::query(
        "DELETE FROM admin_sessions WHERE idle_expires_at<=? OR absolute_expires_at<=?",
    )
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(r.rows_affected())
}

/// 会话只读列表（管理页；无 token 明文）。
#[derive(Debug, Clone, serde::Serialize)]
pub struct SessionView {
    pub admin_id: String,
    pub created_at: i64,
    pub last_seen_at: i64,
    pub idle_expires_at: i64,
    pub absolute_expires_at: i64,
    pub ip: Option<String>,
}

pub async fn list_for_admin(pool: &SqlitePool, admin_id: &str) -> Result<Vec<SessionView>> {
    let rows = sqlx::query(
        "SELECT admin_id,created_at,last_seen_at,idle_expires_at,absolute_expires_at,ip
         FROM admin_sessions WHERE admin_id=? ORDER BY last_seen_at DESC",
    )
    .bind(admin_id)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| SessionView {
            admin_id: r.get("admin_id"),
            created_at: r.get("created_at"),
            last_seen_at: r.get("last_seen_at"),
            idle_expires_at: r.get("idle_expires_at"),
            absolute_expires_at: r.get("absolute_expires_at"),
            ip: r.get("ip"),
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    #[tokio::test]
    async fn create_lookup_expire_revoke() {
        let path = std::env::temp_dir().join(format!("sbm-sess-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let aid = store::admins::create(&pool, "root", "h", "admin")
            .await
            .unwrap();
        let (sid, csrf) = create(&pool, &aid, 3600, 43200, Some("127.0.0.1"), Some("ua"))
            .await
            .unwrap();
        let sid_hash = crate::pki::sha256_hex(sid.as_bytes());
        let now = now_unix();
        // 明文 token 不入库（查不到明文）。
        let s = lookup_valid(&pool, &sid_hash, now).await.unwrap().unwrap();
        assert_eq!(s.admin_id, aid);
        assert_eq!(s.csrf_hash, crate::pki::sha256_hex(csrf.as_bytes()));

        // idle 过期 → 查不到。
        assert!(lookup_valid(&pool, &sid_hash, now + 4000)
            .await
            .unwrap()
            .is_none());
        // GC 清理过期。
        assert_eq!(gc_expired(&pool, now + 100000).await.unwrap(), 1);

        // 建两个再按 admin 批量吊销。
        create(&pool, &aid, 3600, 43200, None, None).await.unwrap();
        create(&pool, &aid, 3600, 43200, None, None).await.unwrap();
        revoke_all_for_admin(&pool, &aid).await.unwrap();
        assert_eq!(list_for_admin(&pool, &aid).await.unwrap().len(), 0);
        pool.close().await;
    }
}

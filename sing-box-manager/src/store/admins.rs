//! admin_users 仓储：管理员账号 + 角色 + 登录节流。密码以 Argon2id PHC 串存（非信封加密）。
//! 密码哈希/校验原语在 `manager::auth`（用 argon2 crate）；store 只存/取 PHC 串与状态。

use serde::Serialize;
use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

#[derive(Debug, Clone)]
pub struct AdminUser {
    pub id: String,
    pub username: String,
    pub password_hash: String,
    pub role: String,
    pub disabled: bool,
    pub password_changed_at: i64,
    pub last_login_at: Option<i64>,
    pub failed_attempts: i64,
    pub locked_until: Option<i64>,
    pub created_at: i64,
    pub updated_at: i64,
}

/// 脱敏投影（管理页/审计；无 password_hash）。
#[derive(Debug, Clone, Serialize)]
pub struct AdminView {
    pub id: String,
    pub username: String,
    pub role: String,
    pub disabled: bool,
    pub last_login_at: Option<i64>,
    pub locked: bool,
    pub created_at: i64,
}

fn row_to_admin(r: &sqlx::sqlite::SqliteRow) -> AdminUser {
    AdminUser {
        id: r.get("id"),
        username: r.get("username"),
        password_hash: r.get("password_hash"),
        role: r.get("role"),
        disabled: r.get::<i64, _>("disabled") != 0,
        password_changed_at: r.get("password_changed_at"),
        last_login_at: r.get("last_login_at"),
        failed_attempts: r.get("failed_attempts"),
        locked_until: r.get("locked_until"),
        created_at: r.get("created_at"),
        updated_at: r.get("updated_at"),
    }
}

/// 管理员总数（首启引导判定）。
pub async fn count(pool: &SqlitePool) -> Result<i64> {
    Ok(sqlx::query_scalar("SELECT COUNT(*) FROM admin_users")
        .fetch_one(pool)
        .await?)
}

/// 创建管理员（password_hash 已由调用方算好）。返回 id。
pub async fn create(
    pool: &SqlitePool,
    username: &str,
    password_hash: &str,
    role: &str,
) -> Result<String> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    sqlx::query(
        "INSERT INTO admin_users(id,username,password_hash,role,disabled,password_changed_at,failed_attempts,created_at,updated_at)
         VALUES(?,?,?,?,0,?,0,?,?)",
    )
    .bind(&id)
    .bind(username)
    .bind(password_hash)
    .bind(role)
    .bind(now)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(id)
}

/// **原子**创建首个管理员：仅当 admin_users 为空时插入（单条 `INSERT..SELECT..WHERE NOT EXISTS`，
/// 防 setup TOCTOU 竞态）。已存在任何管理员则不插入并返回 None。
pub async fn create_if_none(
    pool: &SqlitePool,
    username: &str,
    password_hash: &str,
    role: &str,
) -> Result<Option<String>> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let res = sqlx::query(
        "INSERT INTO admin_users(id,username,password_hash,role,disabled,password_changed_at,failed_attempts,created_at,updated_at)
         SELECT ?,?,?,?,0,?,0,?,? WHERE NOT EXISTS(SELECT 1 FROM admin_users)",
    )
    .bind(&id)
    .bind(username)
    .bind(password_hash)
    .bind(role)
    .bind(now)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok((res.rows_affected() == 1).then_some(id))
}

pub async fn get_by_username(pool: &SqlitePool, username: &str) -> Result<Option<AdminUser>> {
    let row = sqlx::query("SELECT * FROM admin_users WHERE username=?")
        .bind(username)
        .fetch_optional(pool)
        .await?;
    Ok(row.as_ref().map(row_to_admin))
}

pub async fn get_by_id(pool: &SqlitePool, id: &str) -> Result<Option<AdminUser>> {
    let row = sqlx::query("SELECT * FROM admin_users WHERE id=?")
        .bind(id)
        .fetch_optional(pool)
        .await?;
    Ok(row.as_ref().map(row_to_admin))
}

pub async fn list(pool: &SqlitePool) -> Result<Vec<AdminView>> {
    let now = now_unix();
    let rows = sqlx::query("SELECT * FROM admin_users ORDER BY username")
        .fetch_all(pool)
        .await?;
    Ok(rows
        .iter()
        .map(|r| {
            let a = row_to_admin(r);
            AdminView {
                id: a.id,
                username: a.username,
                role: a.role,
                disabled: a.disabled,
                last_login_at: a.last_login_at,
                locked: a.locked_until.map(|t| t > now).unwrap_or(false),
                created_at: a.created_at,
            }
        })
        .collect())
}

/// 改密：更新 hash + password_changed_at（使旧会话作废）+ 解锁。
pub async fn set_password(pool: &SqlitePool, id: &str, new_hash: &str) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "UPDATE admin_users SET password_hash=?, password_changed_at=?, failed_attempts=0, locked_until=NULL, updated_at=? WHERE id=?",
    )
    .bind(new_hash)
    .bind(now)
    .bind(now)
    .bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn set_role(pool: &SqlitePool, id: &str, role: &str) -> Result<()> {
    sqlx::query("UPDATE admin_users SET role=?, updated_at=? WHERE id=?")
        .bind(role)
        .bind(now_unix())
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn set_disabled(pool: &SqlitePool, id: &str, disabled: bool) -> Result<()> {
    sqlx::query("UPDATE admin_users SET disabled=?, updated_at=? WHERE id=?")
        .bind(disabled as i64)
        .bind(now_unix())
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

/// 登录成功：记 last_login_at，清零失败计数与锁。
pub async fn note_login_ok(pool: &SqlitePool, id: &str) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "UPDATE admin_users SET last_login_at=?, failed_attempts=0, locked_until=NULL, updated_at=? WHERE id=?",
    )
    .bind(now)
    .bind(now)
    .bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

/// 登录失败：失败计数 +1；达阈值置锁定窗口。
pub async fn note_login_fail(
    pool: &SqlitePool,
    id: &str,
    lock_threshold: i64,
    lock_secs: i64,
) -> Result<()> {
    let now = now_unix();
    // 原子自增并在达阈值时置锁。
    sqlx::query(
        "UPDATE admin_users SET failed_attempts=failed_attempts+1,
            locked_until=CASE WHEN failed_attempts+1 >= ? THEN ? ELSE locked_until END,
            updated_at=? WHERE id=?",
    )
    .bind(lock_threshold)
    .bind(now + lock_secs)
    .bind(now)
    .bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-admins-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }

    #[tokio::test]
    async fn crud_and_lockout() {
        let pool = pool().await;
        assert_eq!(count(&pool).await.unwrap(), 0);
        let id = create(&pool, "root", "phc$hash", "admin").await.unwrap();
        assert_eq!(count(&pool).await.unwrap(), 1);
        let a = get_by_username(&pool, "root").await.unwrap().unwrap();
        assert_eq!(a.role, "admin");
        assert!(!a.disabled);

        // 失败 3 次锁定（阈值 3）。
        for _ in 0..3 {
            note_login_fail(&pool, &id, 3, 900).await.unwrap();
        }
        let a = get_by_id(&pool, &id).await.unwrap().unwrap();
        assert_eq!(a.failed_attempts, 3);
        assert!(a.locked_until.unwrap() > now_unix());
        // 登录成功清零。
        note_login_ok(&pool, &id).await.unwrap();
        let a = get_by_id(&pool, &id).await.unwrap().unwrap();
        assert_eq!(a.failed_attempts, 0);
        assert!(a.locked_until.is_none());
        assert!(a.last_login_at.is_some());

        // 改密推进 password_changed_at。
        let before = a.password_changed_at;
        std::thread::sleep(std::time::Duration::from_millis(1100));
        set_password(&pool, &id, "phc$new").await.unwrap();
        let a = get_by_id(&pool, &id).await.unwrap().unwrap();
        assert!(a.password_changed_at >= before);
        assert_eq!(a.password_hash, "phc$new");
        pool.close().await;
    }

    #[tokio::test]
    async fn create_if_none_is_atomic_first_admin_only() {
        let pool = pool().await;
        // 首个成功。
        let first = create_if_none(&pool, "a", "h", "admin").await.unwrap();
        assert!(first.is_some());
        // 已有管理员 → 不再插入（即使不同用户名），返回 None（防 setup 竞态建第二 admin）。
        let second = create_if_none(&pool, "b", "h", "admin").await.unwrap();
        assert!(second.is_none());
        assert_eq!(count(&pool).await.unwrap(), 1);
        pool.close().await;
    }
}

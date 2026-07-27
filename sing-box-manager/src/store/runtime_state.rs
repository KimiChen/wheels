//! user_runtime_state / entry_runtime_state 读写。配额/到期评估产出 `effective_disabled`，供 eligible_desired
//! 廉价排除；仅在资格翻转时触发 reconcile。entry_runtime_state 承载计量故障隔离与过期告警观测。

use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

/// 一次配额评估的结果（供 tick 决定是否翻转触发 reconcile）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserEval {
    pub user_id: String,
    pub effective_disabled: bool,
    pub quota_state: String, // ok / over / expired
    pub active_period: String,
    pub flipped: bool, // effective_disabled 是否相对上次变化
}

/// 评估某用户当前资格并写 user_runtime_state。返回是否翻转。
pub async fn evaluate_user(
    pool: &SqlitePool,
    user_id: &str,
    active_period: &str,
    used_bytes: i64,
    quota_bytes: i64,
    expire_at: Option<i64>,
    now: i64,
) -> Result<UserEval> {
    let over = quota_bytes > 0 && used_bytes >= quota_bytes;
    let expired = expire_at.map(|e| now >= e).unwrap_or(false);
    let effective_disabled = over || expired;
    let quota_state = if expired {
        "expired"
    } else if over {
        "over"
    } else {
        "ok"
    };

    let prev: Option<i64> =
        sqlx::query_scalar("SELECT effective_disabled FROM user_runtime_state WHERE user_id=?")
            .bind(user_id)
            .fetch_optional(pool)
            .await?;
    // 首次评估默认视为「未停用」——ok→ok 不算翻转（授权时已入 SSM），仅 over/expired 才需 reconcile。
    let prev_disabled = prev.map(|v| v != 0).unwrap_or(false);
    let flipped = prev_disabled != effective_disabled;

    sqlx::query(
        "INSERT INTO user_runtime_state(user_id,active_period,quota_state,effective_disabled,over_since,last_evaluated_at,updated_at)
         VALUES(?,?,?,?,?,?,?)
         ON CONFLICT(user_id) DO UPDATE SET
            active_period=excluded.active_period, quota_state=excluded.quota_state,
            effective_disabled=excluded.effective_disabled,
            over_since=CASE WHEN excluded.effective_disabled=1 AND user_runtime_state.effective_disabled=0
                            THEN excluded.over_since ELSE
                            (CASE WHEN excluded.effective_disabled=0 THEN NULL ELSE user_runtime_state.over_since END) END,
            last_evaluated_at=excluded.last_evaluated_at, updated_at=excluded.updated_at",
    )
    .bind(user_id)
    .bind(active_period)
    .bind(quota_state)
    .bind(effective_disabled as i64)
    .bind(if effective_disabled { Some(now) } else { None })
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;

    Ok(UserEval {
        user_id: user_id.to_string(),
        effective_disabled,
        quota_state: quota_state.to_string(),
        active_period: active_period.to_string(),
        flipped,
    })
}

/// user_runtime_state 读视图。
#[derive(Debug, Clone, serde::Serialize)]
pub struct UserRuntimeState {
    pub active_period: String,
    pub quota_state: String,
    pub effective_disabled: bool,
    pub over_since: Option<i64>,
}

pub async fn get_user_state(pool: &SqlitePool, user_id: &str) -> Result<Option<UserRuntimeState>> {
    let row = sqlx::query(
        "SELECT active_period,quota_state,effective_disabled,over_since FROM user_runtime_state WHERE user_id=?",
    )
    .bind(user_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| UserRuntimeState {
        active_period: r.get("active_period"),
        quota_state: r.get("quota_state"),
        effective_disabled: r.get::<i64, _>("effective_disabled") != 0,
        over_since: r.get("over_since"),
    }))
}

// ---------- entry_runtime_state（计量观测/故障隔离/过期）----------

pub async fn record_stats_ok(pool: &SqlitePool, entry_id: &str, boot_id: i64) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "INSERT INTO entry_runtime_state(entry_id,last_stats_attempt_at,last_stats_at,last_reported_boot_id,last_error,consecutive_failures,stale,updated_at)
         VALUES(?,?,?,?,NULL,0,0,?)
         ON CONFLICT(entry_id) DO UPDATE SET last_stats_attempt_at=excluded.last_stats_attempt_at,
            last_stats_at=excluded.last_stats_at, last_reported_boot_id=excluded.last_reported_boot_id,
            last_error=NULL, consecutive_failures=0, stale=0, updated_at=excluded.updated_at",
    )
    .bind(entry_id)
    .bind(now)
    .bind(now)
    .bind(boot_id)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn record_stats_err(pool: &SqlitePool, entry_id: &str, error: &str) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "INSERT INTO entry_runtime_state(entry_id,last_stats_attempt_at,last_error,consecutive_failures,updated_at)
         VALUES(?,?,?,1,?)
         ON CONFLICT(entry_id) DO UPDATE SET last_stats_attempt_at=excluded.last_stats_attempt_at,
            last_error=excluded.last_error, consecutive_failures=entry_runtime_state.consecutive_failures+1,
            updated_at=excluded.updated_at",
    )
    .bind(entry_id)
    .bind(now)
    .bind(error)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

/// 标记过期（now - last_stats_at > stale_secs）；返回新变过期的 entry。
pub async fn mark_stale(pool: &SqlitePool, stale_secs: i64) -> Result<Vec<String>> {
    let now = now_unix();
    let rows = sqlx::query(
        "SELECT entry_id FROM entry_runtime_state WHERE stale=0 AND last_stats_at IS NOT NULL AND ?-last_stats_at>?",
    )
    .bind(now)
    .bind(stale_secs)
    .fetch_all(pool)
    .await?;
    let ids: Vec<String> = rows
        .iter()
        .map(|r| r.get::<String, _>("entry_id"))
        .collect();
    for id in &ids {
        sqlx::query("UPDATE entry_runtime_state SET stale=1, updated_at=? WHERE entry_id=?")
            .bind(now)
            .bind(id)
            .execute(pool)
            .await?;
    }
    Ok(ids)
}

#[derive(Debug, Clone, serde::Serialize)]
pub struct EntryRuntimeState {
    pub entry_id: String,
    pub last_stats_attempt_at: Option<i64>,
    pub last_stats_at: Option<i64>,
    pub last_reported_boot_id: Option<i64>,
    pub last_error: Option<String>,
    pub consecutive_failures: i64,
    pub stale: bool,
}

pub async fn list_entry_states(pool: &SqlitePool) -> Result<Vec<EntryRuntimeState>> {
    let rows = sqlx::query(
        "SELECT entry_id,last_stats_attempt_at,last_stats_at,last_reported_boot_id,last_error,consecutive_failures,stale FROM entry_runtime_state ORDER BY entry_id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| EntryRuntimeState {
            entry_id: r.get("entry_id"),
            last_stats_attempt_at: r.get("last_stats_attempt_at"),
            last_stats_at: r.get("last_stats_at"),
            last_reported_boot_id: r.get("last_reported_boot_id"),
            last_error: r.get("last_error"),
            consecutive_failures: r.get("consecutive_failures"),
            stale: r.get::<i64, _>("stale") != 0,
        })
        .collect())
}

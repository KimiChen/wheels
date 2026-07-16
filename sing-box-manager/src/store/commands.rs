//! agent_commands / agent_command_results 仓储。命令幂等的四段全部编码为**原子 SQL**，而非
//! check-then-act：Manager 创建 = INSERT ON CONFLICT DO NOTHING + SELECT；派发领取 = 条件 UPDATE
//! 单飞；完成 = 写终态 + 结果；重试 = 退避重排。保证并发/重启下不重复派发、不重复副作用。

use crate::domain::agent::{CommandKind, CommandStatus};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::now_unix;
use sqlx::{Row, SqlitePool};

/// 创建命令的输入。
pub struct NewCommand<'a> {
    pub host_id: &'a str,
    pub kind: CommandKind,
    pub idempotency_key: &'a str,
    pub request_hash: &'a str,
    pub request_json: &'a str,
    pub max_attempts: i64,
    pub timeout_ms: i64,
}

/// 命令行的读视图（调度器用）。
#[derive(Debug, Clone)]
pub struct CommandRow {
    pub command_id: String,
    pub host_id: String,
    pub kind: String,
    pub request_json: String,
    pub status: String,
    pub attempts: i64,
    pub max_attempts: i64,
    pub timeout_ms: i64,
    pub not_before: i64,
    pub deadline_at: Option<i64>,
}

/// 命令结果的写入/读取视图（入库前须脱敏；禁明文密钥）。
#[derive(Debug, Clone)]
pub struct CommandResult {
    pub ok: bool,
    pub http_status: Option<i64>,
    pub result_json: Option<String>,
    pub agent_echo_command_id: Option<String>,
    pub error_code: Option<String>,
    pub error_message: Option<String>,
}

fn row_to_command(row: &sqlx::sqlite::SqliteRow) -> CommandRow {
    CommandRow {
        command_id: row.get("command_id"),
        host_id: row.get("host_id"),
        kind: row.get("kind"),
        request_json: row.get("request_json"),
        status: row.get("status"),
        attempts: row.get("attempts"),
        max_attempts: row.get("max_attempts"),
        timeout_ms: row.get("timeout_ms"),
        not_before: row.get("not_before"),
        deadline_at: row.get("deadline_at"),
    }
}

/// (a) 创建或取回。INSERT ON CONFLICT(host_id,idempotency_key) DO NOTHING，再 SELECT 幸存行。
/// 两个并发调度收敛到同一 command_id；已存在但 request_hash 不同 → Conflict(409)。
pub async fn create_or_get(pool: &SqlitePool, cmd_id: &str, nc: &NewCommand<'_>) -> Result<String> {
    let now = now_unix();
    sqlx::query(
        "INSERT INTO agent_commands(command_id,host_id,kind,idempotency_key,request_hash,request_json,status,attempts,max_attempts,timeout_ms,not_before,created_at,updated_at)
         VALUES(?,?,?,?,?,?,'pending',0,?,?,0,?,?)
         ON CONFLICT(host_id,idempotency_key) DO NOTHING",
    )
    .bind(cmd_id)
    .bind(nc.host_id)
    .bind(nc.kind.as_str())
    .bind(nc.idempotency_key)
    .bind(nc.request_hash)
    .bind(nc.request_json)
    .bind(nc.max_attempts)
    .bind(nc.timeout_ms)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;

    let row = sqlx::query(
        "SELECT command_id, request_hash FROM agent_commands WHERE host_id=? AND idempotency_key=?",
    )
    .bind(nc.host_id)
    .bind(nc.idempotency_key)
    .fetch_one(pool)
    .await?;
    let existing_hash: String = row.get("request_hash");
    if existing_hash != nc.request_hash {
        return Err(AppError::new(
            ErrorCode::Conflict,
            "同一 idempotency_key 的请求体不一致",
        ));
    }
    Ok(row.get("command_id"))
}

/// (b) 派发领取（single-flight）。仅当 status='pending' 时置 in_flight 并自增 attempts。
/// 返回是否成功领取（rows_affected==1）。
pub async fn claim_for_dispatch(pool: &SqlitePool, command_id: &str) -> Result<bool> {
    let now = now_unix();
    let res = sqlx::query(
        "UPDATE agent_commands
         SET status='in_flight', attempts=attempts+1, dispatched_at=?, deadline_at=? + timeout_ms/1000, updated_at=?
         WHERE command_id=? AND status='pending'",
    )
    .bind(now)
    .bind(now)
    .bind(now)
    .bind(command_id)
    .execute(pool)
    .await?;
    Ok(res.rows_affected() == 1)
}

/// (c) 退避重排：把仍在 in_flight 的命令改回 pending 并设 not_before（下次可派发时间）。
pub async fn requeue(pool: &SqlitePool, command_id: &str, not_before: i64) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "UPDATE agent_commands SET status='pending', not_before=?, updated_at=? WHERE command_id=? AND status='in_flight'",
    )
    .bind(not_before)
    .bind(now)
    .bind(command_id)
    .execute(pool)
    .await?;
    Ok(())
}

/// (d) 完成：写终态 + 结果（脱敏）。单事务。
pub async fn complete(
    pool: &SqlitePool,
    command_id: &str,
    status: CommandStatus,
    result: &CommandResult,
) -> Result<()> {
    debug_assert!(status.is_terminal());
    let now = now_unix();
    let mut tx = pool.begin().await?;
    sqlx::query(
        "UPDATE agent_commands SET status=?, completed_at=?, updated_at=? WHERE command_id=?",
    )
    .bind(status.as_str())
    .bind(now)
    .bind(now)
    .bind(command_id)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "INSERT OR REPLACE INTO agent_command_results(command_id,ok,http_status,result_json,agent_echo_command_id,error_code,error_message,observed_at)
         VALUES(?,?,?,?,?,?,?,?)",
    )
    .bind(command_id)
    .bind(result.ok as i64)
    .bind(result.http_status)
    .bind(result.result_json.as_deref())
    .bind(result.agent_echo_command_id.as_deref())
    .bind(result.error_code.as_deref())
    .bind(result.error_message.as_deref())
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(())
}

pub async fn fetch_command(pool: &SqlitePool, command_id: &str) -> Result<Option<CommandRow>> {
    let row = sqlx::query(
        "SELECT command_id,host_id,kind,request_json,status,attempts,max_attempts,timeout_ms,not_before,deadline_at
         FROM agent_commands WHERE command_id=?",
    )
    .bind(command_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_command))
}

pub async fn get_result(pool: &SqlitePool, command_id: &str) -> Result<Option<CommandResult>> {
    let row = sqlx::query(
        "SELECT ok,http_status,result_json,agent_echo_command_id,error_code,error_message FROM agent_command_results WHERE command_id=?",
    )
    .bind(command_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| CommandResult {
        ok: r.get::<i64, _>("ok") != 0,
        http_status: r.get("http_status"),
        result_json: r.get("result_json"),
        agent_echo_command_id: r.get("agent_echo_command_id"),
        error_code: r.get("error_code"),
        error_message: r.get("error_message"),
    }))
}

/// 重启恢复：列出未终结命令（pending / in_flight），供对账（GET-then-repost）。
pub async fn list_recoverable(pool: &SqlitePool) -> Result<Vec<CommandRow>> {
    let rows = sqlx::query(
        "SELECT command_id,host_id,kind,request_json,status,attempts,max_attempts,timeout_ms,not_before,deadline_at
         FROM agent_commands WHERE status IN ('pending','in_flight') ORDER BY created_at",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_command).collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::store;

    async fn pool_with_host() -> (sqlx::SqlitePool, String) {
        let path = std::env::temp_dir().join(format!("sbm-cmd-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let host = store::hosts::create_host(&pool, "h", None, &[Capability::Entry])
            .await
            .unwrap();
        (pool, host)
    }

    fn nc<'a>(host: &'a str, key: &'a str, hash: &'a str) -> NewCommand<'a> {
        NewCommand {
            host_id: host,
            kind: CommandKind::Status,
            idempotency_key: key,
            request_hash: hash,
            request_json: "{}",
            max_attempts: 3,
            timeout_ms: 15000,
        }
    }

    #[tokio::test]
    async fn create_is_idempotent_and_conflict_on_hash_mismatch() {
        let (pool, host) = pool_with_host().await;
        let id1 = create_or_get(&pool, "cmd-a", &nc(&host, "k1", "h1"))
            .await
            .unwrap();
        // 不同 command_id 但同 (host, idempotency_key) → 收敛到 id1。
        let id2 = create_or_get(&pool, "cmd-b", &nc(&host, "k1", "h1"))
            .await
            .unwrap();
        assert_eq!(id1, id2);
        // 同 key 不同 request_hash → Conflict。
        let err = create_or_get(&pool, "cmd-c", &nc(&host, "k1", "h2"))
            .await
            .unwrap_err();
        assert_eq!(err.code, ErrorCode::Conflict);
        pool.close().await;
    }

    #[tokio::test]
    async fn claim_is_single_flight() {
        let (pool, host) = pool_with_host().await;
        let id = create_or_get(&pool, "cmd-x", &nc(&host, "k", "h"))
            .await
            .unwrap();
        assert!(claim_for_dispatch(&pool, &id).await.unwrap(), "首次应领取");
        assert!(
            !claim_for_dispatch(&pool, &id).await.unwrap(),
            "已 in_flight 不能再领取"
        );
        // 退避重排后可再次领取。
        requeue(&pool, &id, 0).await.unwrap();
        assert!(claim_for_dispatch(&pool, &id).await.unwrap());
        pool.close().await;
    }

    #[tokio::test]
    async fn complete_writes_terminal_and_result() {
        let (pool, host) = pool_with_host().await;
        let id = create_or_get(&pool, "cmd-r", &nc(&host, "k", "h"))
            .await
            .unwrap();
        claim_for_dispatch(&pool, &id).await.unwrap();
        complete(
            &pool,
            &id,
            CommandStatus::Succeeded,
            &CommandResult {
                ok: true,
                http_status: Some(200),
                result_json: Some("{\"ok\":true}".into()),
                agent_echo_command_id: Some(id.clone()),
                error_code: None,
                error_message: None,
            },
        )
        .await
        .unwrap();
        let cmd = fetch_command(&pool, &id).await.unwrap().unwrap();
        assert_eq!(cmd.status, "succeeded");
        let res = get_result(&pool, &id).await.unwrap().unwrap();
        assert!(res.ok && res.http_status == Some(200));
        // 终态命令不再可领取。
        assert!(!claim_for_dispatch(&pool, &id).await.unwrap());
        pool.close().await;
    }
}

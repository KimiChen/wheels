//! execute_once：Agent 侧命令幂等的原子抢占（闭 TOCTOU）。
//! `INSERT ... ON CONFLICT(command_id) DO NOTHING` 抢占锁行——赢者执行预定义操作并写终态，
//! 输者按 request_hash 与状态分流为回放 / 进行中 / 409。两个同 id 并发请求绝不双执行。

use std::future::Future;

use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

#[derive(Debug, Clone)]
pub struct ExecResult {
    pub ok: bool,
    pub http_status: u16,
    pub body_json: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Executed,
    Replayed,
    InProgress,
    Conflict,
}

pub struct ExecOnce {
    pub outcome: Outcome,
    pub result: Option<ExecResult>,
}

/// 原子执行一次。`op` 只在赢得抢占时被 await（输者不执行副作用）。
pub async fn execute_once<F>(
    pool: &SqlitePool,
    command_id: &str,
    kind: &str,
    request_hash: &str,
    op: F,
) -> Result<ExecOnce>
where
    F: Future<Output = Result<ExecResult>>,
{
    let now = now_unix();
    let inserted = sqlx::query(
        "INSERT INTO executed_commands(command_id,kind,request_hash,status,created_at)
         VALUES(?,?,?,'in_flight',?) ON CONFLICT(command_id) DO NOTHING",
    )
    .bind(command_id)
    .bind(kind)
    .bind(request_hash)
    .bind(now)
    .execute(pool)
    .await?;

    if inserted.rows_affected() == 1 {
        // 赢得抢占：执行预定义操作并写终态。
        let result = op.await?;
        let status = if result.ok { "succeeded" } else { "failed" };
        sqlx::query(
            "UPDATE executed_commands SET status=?, result_json=?, http_status=?, completed_at=? WHERE command_id=?",
        )
        .bind(status)
        .bind(&result.body_json)
        .bind(result.http_status as i64)
        .bind(now_unix())
        .bind(command_id)
        .execute(pool)
        .await?;
        return Ok(ExecOnce {
            outcome: Outcome::Executed,
            result: Some(result),
        });
    }

    // 冲突：读既有行分流。
    let row = sqlx::query(
        "SELECT request_hash,status,result_json,http_status FROM executed_commands WHERE command_id=?",
    )
    .bind(command_id)
    .fetch_one(pool)
    .await?;
    let existing_hash: String = row.get("request_hash");
    if existing_hash != request_hash {
        return Ok(ExecOnce {
            outcome: Outcome::Conflict,
            result: None,
        });
    }
    let status: String = row.get("status");
    if status == "in_flight" {
        return Ok(ExecOnce {
            outcome: Outcome::InProgress,
            result: None,
        });
    }
    Ok(ExecOnce {
        outcome: Outcome::Replayed,
        result: Some(ExecResult {
            ok: status == "succeeded",
            http_status: row.get::<Option<i64>, _>("http_status").unwrap_or(200) as u16,
            body_json: row
                .get::<Option<String>, _>("result_json")
                .unwrap_or_default(),
        }),
    })
}

/// 查已记录命令结果（GET /v1/deployments/{id}）。
pub async fn get_executed(pool: &SqlitePool, command_id: &str) -> Result<Option<ExecResult>> {
    let row = sqlx::query(
        "SELECT status,result_json,http_status FROM executed_commands WHERE command_id=?",
    )
    .bind(command_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| {
        let status: String = r.get("status");
        ExecResult {
            ok: status == "succeeded",
            http_status: r.get::<Option<i64>, _>("http_status").unwrap_or(202) as u16,
            body_json: r
                .get::<Option<String>, _>("result_json")
                .unwrap_or_default(),
        }
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::state;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;

    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-idem-{}.db", uuid::Uuid::new_v4()));
        state::open(&path.to_string_lossy()).await.unwrap()
    }

    async fn stub(counter: Arc<AtomicUsize>) -> Result<ExecResult> {
        counter.fetch_add(1, Ordering::SeqCst);
        Ok(ExecResult {
            ok: true,
            http_status: 200,
            body_json: "{\"accepted\":true}".into(),
        })
    }

    #[tokio::test]
    async fn executes_once_then_replays_without_rerunning() {
        let pool = pool().await;
        let counter = Arc::new(AtomicUsize::new(0));

        let r1 = execute_once(&pool, "c1", "reconcile", "h1", stub(counter.clone()))
            .await
            .unwrap();
        assert_eq!(r1.outcome, Outcome::Executed);
        assert_eq!(counter.load(Ordering::SeqCst), 1);

        // 同 id 同 hash：回放，op 不再执行。
        let r2 = execute_once(&pool, "c1", "reconcile", "h1", stub(counter.clone()))
            .await
            .unwrap();
        assert_eq!(r2.outcome, Outcome::Replayed);
        assert_eq!(counter.load(Ordering::SeqCst), 1, "回放不得重复执行");
        assert_eq!(r2.result.unwrap().body_json, r1.result.unwrap().body_json);

        // 同 id 异 hash：409。
        let r3 = execute_once(&pool, "c1", "reconcile", "hDIFF", stub(counter.clone()))
            .await
            .unwrap();
        assert_eq!(r3.outcome, Outcome::Conflict);
        assert_eq!(counter.load(Ordering::SeqCst), 1);
        pool.close().await;
    }

    #[tokio::test]
    async fn in_flight_row_reports_in_progress() {
        let pool = pool().await;
        // 手动插入 in_flight 行模拟并发中的另一请求。
        sqlx::query(
            "INSERT INTO executed_commands(command_id,kind,request_hash,status,created_at) VALUES('c2','deploy','h','in_flight',0)",
        )
        .execute(&pool)
        .await
        .unwrap();
        let counter = Arc::new(AtomicUsize::new(0));
        let r = execute_once(&pool, "c2", "deploy", "h", stub(counter.clone()))
            .await
            .unwrap();
        assert_eq!(r.outcome, Outcome::InProgress);
        assert_eq!(counter.load(Ordering::SeqCst), 0);
        pool.close().await;
    }
}

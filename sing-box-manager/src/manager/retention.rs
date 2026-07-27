//! 数据保留：定期批量裁剪历史/观测表。**白名单**——只删 append-only 历史流，权威态/用量/基线永不删。
//! 批量小事务 + 批间让出写者（4 连接池预算）。保留天数经 settings 运行期可调。

use std::time::Duration;

use sqlx::SqlitePool;
use tokio_util::sync::CancellationToken;

use crate::error::Result;
use crate::store::{now_unix, settings};

const DAY: i64 = 86400;

/// 一张可裁表：时间列 + settings 保留键 + 默认天数 + 额外条件（如仅终态命令）。
struct Target {
    table: &'static str,
    time_col: &'static str,
    key: &'static str,
    default_days: i64,
    extra: &'static str,
}

const TARGETS: &[Target] = &[
    Target {
        table: "audit_logs",
        time_col: "created_at",
        key: "retention_audit_days",
        default_days: 365,
        extra: "",
    },
    Target {
        table: "health_events",
        time_col: "created_at",
        key: "retention_health_days",
        default_days: 30,
        extra: "",
    },
    Target {
        table: "agent_status_snapshots",
        time_col: "polled_at",
        key: "retention_snapshots_days",
        default_days: 14,
        extra: "",
    },
    // 仅终态命令（completed_at 非空）；agent_command_results 经 FK CASCADE 连带清理。
    Target {
        table: "agent_commands",
        time_col: "completed_at",
        key: "retention_commands_days",
        default_days: 30,
        extra: "AND completed_at IS NOT NULL",
    },
    // traffic_batches 是结算精确一次台账——保留期须远大于任何重投窗口（默认 90 天）。
    Target {
        table: "traffic_batches",
        time_col: "ingested_at",
        key: "retention_batches_days",
        default_days: 90,
        extra: "",
    },
];

/// 跑一轮保留裁剪，返回各表删除条数。
pub async fn prune_once(pool: &SqlitePool) -> Result<Vec<(&'static str, u64)>> {
    let now = now_unix();
    let batch = settings::get_i64(pool, "retention_batch_size", 500)
        .await?
        .clamp(50, 5000);
    let mut out = Vec::new();
    for t in TARGETS {
        let days = settings::get_i64(pool, t.key, t.default_days).await?.max(1);
        let cutoff = now - days * DAY;
        let deleted = prune_table(pool, t, cutoff, batch).await?;
        if deleted > 0 {
            tracing::info!(table = t.table, deleted, "保留裁剪");
        }
        out.push((t.table, deleted));
    }
    Ok(out)
}

async fn prune_table(pool: &SqlitePool, t: &Target, cutoff: i64, batch: i64) -> Result<u64> {
    // table/col/extra 均为编译期常量（非用户输入），字符串拼接无注入面。
    let sql = format!(
        "DELETE FROM {tbl} WHERE rowid IN (SELECT rowid FROM {tbl} WHERE {col} < ? {extra} LIMIT ?)",
        tbl = t.table,
        col = t.time_col,
        extra = t.extra,
    );
    let mut total = 0u64;
    loop {
        let n = sqlx::query(&sql)
            .bind(cutoff)
            .bind(batch)
            .execute(pool)
            .await?
            .rows_affected();
        total += n;
        if n < batch as u64 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await; // 让出写者
    }
    Ok(total)
}

/// 后台保留循环。周期经 settings `retention_interval_secs`（默认 6h）。随 cancel 退出。
pub async fn prune_loop(pool: SqlitePool, cancel: CancellationToken) {
    // 首次延迟一小段再跑，避开启动高峰。
    let mut tick = tokio::time::interval(Duration::from_secs(
        settings::get_i64(&pool, "retention_interval_secs", 21600)
            .await
            .unwrap_or(21600)
            .clamp(300, 604800) as u64,
    ));
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tick.tick() => {
                if let Err(e) = prune_once(&pool).await {
                    tracing::warn!(error = %e, "保留裁剪失败");
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    #[tokio::test]
    async fn prunes_old_history_keeps_recent_and_authoritative() {
        let path = std::env::temp_dir().join(format!("sbm-ret-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let now = now_unix();
        let old = now - 400 * DAY; // 早于所有保留窗口
                                   // 旧 + 新 审计各一。
        sqlx::query("INSERT INTO audit_logs(id,action,created_at) VALUES('a1','x',?)")
            .bind(old)
            .execute(&pool)
            .await
            .unwrap();
        sqlx::query("INSERT INTO audit_logs(id,action,created_at) VALUES('a2','y',?)")
            .bind(now)
            .execute(&pool)
            .await
            .unwrap();
        // 旧 health_event（30 天保留 → 400 天前该删）。
        store::agents::insert_health_event(&pool, None, "k", None)
            .await
            .unwrap();
        sqlx::query("UPDATE health_events SET created_at=?")
            .bind(old)
            .execute(&pool)
            .await
            .unwrap();
        // 权威用量桶不受裁剪影响。
        sqlx::query("INSERT INTO users(id,name,quota_bytes,reset_cycle,disabled,created_at,updated_at) VALUES('u','n',0,'monthly',0,?,?)")
            .bind(now).bind(now).execute(&pool).await.unwrap();
        sqlx::query("INSERT INTO usage_buckets(user_id,period,uplink_bytes,downlink_bytes,updated_at) VALUES('u','2020-01',1,2,?)")
            .bind(old)
            .execute(&pool)
            .await
            .unwrap();

        let res = prune_once(&pool).await.unwrap();
        let audit_deleted = res.iter().find(|(t, _)| *t == "audit_logs").unwrap().1;
        let health_deleted = res.iter().find(|(t, _)| *t == "health_events").unwrap().1;
        assert_eq!(audit_deleted, 1, "只删超龄审计");
        assert_eq!(health_deleted, 1);
        // 新审计仍在。
        let remain: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM audit_logs")
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(remain, 1);
        // 用量桶纹丝不动。
        let ub: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM usage_buckets")
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(ub, 1);
        pool.close().await;
    }
}

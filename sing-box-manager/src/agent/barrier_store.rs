//! 结算屏障暂存仓储（Agent 本地库）：meter_outbox（最终统计，Manager ack 前不清）+ pending_barrier
//! （待 phase B 完成的部署）。仅存字节/会话数与 revision 指针，绝无 uPSK/明文密钥。

use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

/// pending_barrier 一行：phase A 登记，phase B（meter-ack）消费。
#[derive(Debug, Clone)]
pub struct PendingBarrier {
    pub command_id: String,
    pub revision: i64,
    pub sha256: String,
    pub config_path: String,
    pub role: String,
    pub entry_id: String,
    pub old_epoch: Option<i64>,
    pub sequence: i64,
    pub new_epoch: i64,
    pub drain_clean: bool,
}

/// **原子**登记一次屏障（H1/#4）：单事务内分配 outbox 序号、写最终统计、写 pending_barrier。
/// 三者同生同灭，杜绝「outbox 落库但 pending 未落」的孤儿行。`pb.sequence` 由本函数回填并返回。
/// 幂等：pending_barrier PK=command_id，重投同 command_id 直接返回既有序号（不重读、不新分配）。
pub async fn stage_barrier(
    pool: &SqlitePool,
    pb: &PendingBarrier,
    payload_json: &str,
) -> Result<i64> {
    let mut tx = pool.begin().await?;
    // command_id 幂等门：已登记 → 复用既有序号，绝不再分配/再写 outbox。
    if let Some(existing) =
        sqlx::query_scalar::<_, i64>("SELECT sequence FROM pending_barrier WHERE command_id=?")
            .bind(&pb.command_id)
            .fetch_optional(&mut *tx)
            .await?
    {
        tx.rollback().await?;
        return Ok(existing);
    }
    let old = pb.old_epoch.unwrap_or(-1);
    let seq: i64 = sqlx::query_scalar(
        "SELECT COALESCE(MAX(sequence),-1)+1 FROM meter_outbox WHERE entry_id=? AND runtime_epoch=?",
    )
    .bind(&pb.entry_id)
    .bind(old)
    .fetch_one(&mut *tx)
    .await?;
    let now = now_unix();
    sqlx::query(
        "INSERT INTO meter_outbox(entry_id,runtime_epoch,sequence,payload_json,acked,created_at)
         VALUES(?,?,?,?,0,?)",
    )
    .bind(&pb.entry_id)
    .bind(old)
    .bind(seq)
    .bind(payload_json)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "INSERT INTO pending_barrier(command_id,revision,sha256,config_path,role,entry_id,old_epoch,sequence,new_epoch,drain_clean,created_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?)",
    )
    .bind(&pb.command_id)
    .bind(pb.revision)
    .bind(&pb.sha256)
    .bind(&pb.config_path)
    .bind(&pb.role)
    .bind(&pb.entry_id)
    .bind(pb.old_epoch)
    .bind(seq)
    .bind(pb.new_epoch)
    .bind(pb.drain_clean as i64)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(seq)
}

/// 读某 outbox 行的 payload。
pub async fn get_outbox_payload(
    pool: &SqlitePool,
    entry_id: &str,
    epoch: i64,
    sequence: i64,
) -> Result<Option<String>> {
    let row = sqlx::query(
        "SELECT payload_json FROM meter_outbox WHERE entry_id=? AND runtime_epoch=? AND sequence=?",
    )
    .bind(entry_id)
    .bind(epoch)
    .bind(sequence)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.get("payload_json")))
}

/// 标记 outbox 行已 ack（phase B 停旧前置：确认 Manager 已收最终统计）。
pub async fn mark_outbox_acked(
    pool: &SqlitePool,
    entry_id: &str,
    epoch: i64,
    sequence: i64,
) -> Result<()> {
    sqlx::query(
        "UPDATE meter_outbox SET acked=1 WHERE entry_id=? AND runtime_epoch=? AND sequence=?",
    )
    .bind(entry_id)
    .bind(epoch)
    .bind(sequence)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn get_pending(pool: &SqlitePool, command_id: &str) -> Result<Option<PendingBarrier>> {
    let row = sqlx::query(
        "SELECT command_id,revision,sha256,config_path,role,entry_id,old_epoch,sequence,new_epoch,drain_clean
         FROM pending_barrier WHERE command_id=?",
    )
    .bind(command_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| PendingBarrier {
        command_id: r.get("command_id"),
        revision: r.get("revision"),
        sha256: r.get("sha256"),
        config_path: r.get("config_path"),
        role: r.get("role"),
        entry_id: r.get("entry_id"),
        old_epoch: r.get("old_epoch"),
        sequence: r.get("sequence"),
        new_epoch: r.get("new_epoch"),
        drain_clean: r.get::<i64, _>("drain_clean") != 0,
    }))
}

pub async fn delete_pending(pool: &SqlitePool, command_id: &str) -> Result<()> {
    sqlx::query("DELETE FROM pending_barrier WHERE command_id=?")
        .bind(command_id)
        .execute(pool)
        .await?;
    Ok(())
}

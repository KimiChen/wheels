//! 计量仓储：增量基线（对 SSM 归零/重启稳健）+ **单事务** ingest（修 legacy meter.rs 半提交）+ 每用户周期累计。
//! 基线键含 Agent 上报的 `singbox_boot_id`（新进程新 boot id → 无此行 → last=0 → delta=cur，结构性无负增量）。

use sqlx::{Row, Sqlite, SqlitePool, Transaction};

use crate::domain::metering::StatsBatch;
use crate::domain::user::ResetCycle;
use crate::error::Result;
use crate::manager::metering::period::period_for;
use crate::store::now_unix;

pub const DEFAULT_RESET_DAY: u8 = 1;

/// 增量 = max(0, cur-last)。真·重启由「新 boot id → 无 baseline 行 → last=0 → delta=cur」结构性处理，
/// 故此处**绝不**在 cur<last 时回退到 delta=cur：同一 boot id 的计数器单调，cur<last 只可能是
/// 结算屏障滞后 final 读与例行 poll 交错造成的陈旧读（poll 已把 last 推过 final 的 C1），
/// 一律记 0，避免把整条累计误当复位而重复计费（并发审查 F1）。
pub fn delta_bytes(cur: i64, last: i64) -> i64 {
    (cur - last).max(0)
}

/// 全局重置日（settings.reset_day，默认 1=日历月）。
pub async fn reset_day(pool: &SqlitePool) -> Result<u8> {
    let v: Option<String> = sqlx::query_scalar("SELECT value FROM settings WHERE key='reset_day'")
        .fetch_optional(pool)
        .await?
        .flatten();
    Ok(v.and_then(|s| s.trim().parse::<u8>().ok())
        .unwrap_or(DEFAULT_RESET_DAY))
}

/// 某用户某周期用量 (up, down)。
pub async fn period_usage(pool: &SqlitePool, user_id: &str, period: &str) -> Result<(i64, i64)> {
    let row = sqlx::query(
        "SELECT uplink_bytes, downlink_bytes FROM usage_buckets WHERE user_id=? AND period=?",
    )
    .bind(user_id)
    .bind(period)
    .fetch_optional(pool)
    .await?;
    Ok(row
        .map(|r| (r.get("uplink_bytes"), r.get("downlink_bytes")))
        .unwrap_or((0, 0)))
}

/// 单事务 ingest 一批 stats（例行 poll 或 final）。final 批经 traffic_batches UNIQUE 去重。
/// 返回受影响的 user_id 集（供 tick 末配额再评估）。
pub async fn ingest_batch(
    pool: &SqlitePool,
    entry_id: &str,
    batch: &StatsBatch,
    kind: &str,
    deployment_id: Option<&str>,
    reset_day: u8,
) -> Result<Vec<String>> {
    let now = now_unix();
    let mut tx = pool.begin().await?;

    if kind == "final" {
        // 精确一次台账：同 (entry,boot_id,sequence) 已入账 → 跳过整批（Manager 崩溃重投 delta 0）。
        let res = sqlx::query(
            "INSERT OR IGNORE INTO traffic_batches(entry_id,singbox_boot_id,sequence,kind,deployment_id,observed_at,ingested_at)
             VALUES(?,?,?,?,?,?,?)",
        )
        .bind(entry_id)
        .bind(batch.singbox_boot_id)
        .bind(batch.sequence)
        .bind(kind)
        .bind(deployment_id)
        .bind(batch.observed_at)
        .bind(now)
        .execute(&mut *tx)
        .await?;
        if res.rows_affected() == 0 {
            tx.rollback().await?;
            return Ok(Vec::new());
        }
    }

    let mut affected = std::collections::BTreeSet::new();
    for u in &batch.users {
        // identity_name → user_id + reset_cycle（孤儿身份跳过）。
        let map = sqlx::query(
            "SELECT ur.user_id AS user_id, us.reset_cycle AS reset_cycle
             FROM user_routes ur JOIN users us ON us.id=ur.user_id WHERE ur.identity_name=?",
        )
        .bind(&u.identity_name)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(map) = map else {
            continue; // 孤儿身份：跳过（tick 层记 health_event）
        };
        let user_id: String = map.get("user_id");
        let cycle =
            ResetCycle::parse(&map.get::<String, _>("reset_cycle")).unwrap_or(ResetCycle::Monthly);
        let period = period_for(batch.observed_at, reset_day, cycle);

        // 读基线 → 算 delta → **先写基线再加用量**（同事务原子，杜绝半提交双计/漏计）。
        let base = sqlx::query(
            "SELECT last_uplink_bytes, last_downlink_bytes FROM traffic_baselines
             WHERE entry_id=? AND inbound_tag=? AND identity_name=? AND singbox_boot_id=?",
        )
        .bind(entry_id)
        .bind(&batch.inbound_tag)
        .bind(&u.identity_name)
        .bind(batch.singbox_boot_id)
        .fetch_optional(&mut *tx)
        .await?;
        let (last_up, last_down) = base
            .map(|r| (r.get("last_uplink_bytes"), r.get("last_downlink_bytes")))
            .unwrap_or((0, 0));
        let d_up = delta_bytes(u.uplink_bytes, last_up);
        let d_down = delta_bytes(u.downlink_bytes, last_down);

        sqlx::query(
            "INSERT INTO traffic_baselines(entry_id,inbound_tag,identity_name,singbox_boot_id,last_uplink_bytes,last_downlink_bytes,observed_at,updated_at)
             VALUES(?,?,?,?,?,?,?,?)
             ON CONFLICT(entry_id,inbound_tag,identity_name,singbox_boot_id) DO UPDATE SET
                last_uplink_bytes=excluded.last_uplink_bytes, last_downlink_bytes=excluded.last_downlink_bytes,
                observed_at=excluded.observed_at, updated_at=excluded.updated_at",
        )
        .bind(entry_id)
        .bind(&batch.inbound_tag)
        .bind(&u.identity_name)
        .bind(batch.singbox_boot_id)
        .bind(u.uplink_bytes)
        .bind(u.downlink_bytes)
        .bind(batch.observed_at)
        .bind(now)
        .execute(&mut *tx)
        .await?;

        if d_up != 0 || d_down != 0 {
            add_usage_tx(&mut tx, &user_id, &period, d_up, d_down, now).await?;
        }
        affected.insert(user_id);
    }
    tx.commit().await?;
    Ok(affected.into_iter().collect())
}

async fn add_usage_tx(
    tx: &mut Transaction<'_, Sqlite>,
    user_id: &str,
    period: &str,
    up: i64,
    down: i64,
    now: i64,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO usage_buckets(user_id,period,uplink_bytes,downlink_bytes,updated_at) VALUES(?,?,?,?,?)
         ON CONFLICT(user_id,period) DO UPDATE SET
            uplink_bytes=uplink_bytes+excluded.uplink_bytes,
            downlink_bytes=downlink_bytes+excluded.downlink_bytes, updated_at=excluded.updated_at",
    )
    .bind(user_id)
    .bind(period)
    .bind(up)
    .bind(down)
    .bind(now)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// 周期策略切换时把旧周期桶余额**移动**（非复制）到新周期桶（monthly↔yearly 来回不丢不双计）。
pub async fn carry_forward(
    pool: &SqlitePool,
    user_id: &str,
    old_period: &str,
    new_period: &str,
) -> Result<()> {
    if old_period == new_period {
        return Ok(());
    }
    let (up, down) = period_usage(pool, user_id, old_period).await?;
    if up == 0 && down == 0 {
        return Ok(());
    }
    let now = now_unix();
    let mut tx = pool.begin().await?;
    add_usage_tx(&mut tx, user_id, new_period, up, down, now).await?;
    sqlx::query("DELETE FROM usage_buckets WHERE user_id=? AND period=?")
        .bind(user_id)
        .bind(old_period)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::metering::StatsUser;

    #[test]
    fn delta_reset_robust() {
        assert_eq!(delta_bytes(100, 40), 60);
        assert_eq!(delta_bytes(40, 40), 0);
        // cur<last（陈旧/乱序读，非复位）→ delta=0，绝不回退到 cur（F1：否则屏障 final 与 poll 交错会重复计费）。
        assert_eq!(delta_bytes(30, 100), 0);
        // 真·复位走「新 boot id 无 baseline 行 → last=0」：delta_bytes(cur, 0)=cur。
        assert_eq!(delta_bytes(30, 0), 30);
        assert_eq!(delta_bytes(0, 0), 0);
    }

    async fn setup() -> (SqlitePool, String, String) {
        use crate::store;
        let path = std::env::temp_dir().join(format!("sbm-meter-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        std::env::set_var(
            "ENCRYPTION_MASTER_KEY",
            base64::Engine::encode(&base64::engine::general_purpose::STANDARD, [9u8; 32]),
        );
        let cipher = crate::crypto::Cipher::from_env(1).unwrap();
        let eh =
            store::hosts::create_host(&pool, "eh", None, &[crate::domain::host::Capability::Entry])
                .await
                .unwrap();
        let nh =
            store::hosts::create_host(&pool, "nh", None, &[crate::domain::host::Capability::Node])
                .await
                .unwrap();
        let e1 = store::topology::create_entry(
            &pool,
            &cipher,
            &store::topology::NewEntry {
                host_id: &eh,
                public_address: "e",
                inbound_kind: crate::domain::topology::InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let n1 = store::topology::create_node(&pool, &cipher, &nh, "n", true)
            .await
            .unwrap();
        let r1 = store::topology::insert_route(
            &pool,
            &crate::domain::topology::RouteDraft {
                id: None,
                label: "r".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: crate::domain::topology::ExitKind::Node,
                exit_node_id: Some(n1),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();
        let (uid, _) = store::users::create_user(&pool, "u", 0, "monthly", None)
            .await
            .unwrap();
        let name = store::users::grant_route(&pool, &cipher, &uid, &r1)
            .await
            .unwrap();
        (pool, e1, name.clone() + "\x00" + &uid) // pack name+uid
    }

    fn batch(name: &str, boot: i64, up: i64, down: i64) -> StatsBatch {
        StatsBatch {
            inbound_tag: "in-shared".into(),
            singbox_boot_id: boot,
            sequence: 0,
            observed_at: 1_700_000_000,
            tcp_sessions: 0,
            udp_sessions: 0,
            users: vec![StatsUser {
                identity_name: name.into(),
                uplink_bytes: up,
                downlink_bytes: down,
            }],
        }
    }

    #[tokio::test]
    async fn ingest_accumulates_delta_idempotently_and_boot_id_resets() {
        let (pool, e1, packed) = setup().await;
        let (name, uid) = packed.split_once('\x00').unwrap();
        let rd = reset_day(&pool).await.unwrap();

        // 首读：cumulative 100/200 → delta 全量。
        let aff = ingest_batch(&pool, &e1, &batch(name, 1, 100, 200), "poll", None, rd)
            .await
            .unwrap();
        assert_eq!(aff, vec![uid.to_string()]);
        let period = period_for(1_700_000_000, rd, ResetCycle::Monthly);
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (100, 200));

        // 再读同 cumulative：delta 0，用量不变（单事务基线幂等）。
        ingest_batch(&pool, &e1, &batch(name, 1, 100, 200), "poll", None, rd)
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (100, 200));

        // cumulative 增长 150/260：delta 50/60。
        ingest_batch(&pool, &e1, &batch(name, 1, 150, 260), "poll", None, rd)
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (150, 260));

        // 新 boot id=2，cumulative 从 0 复位到 30/40：新基线 last=0 → delta 30/40，无负增量。
        ingest_batch(&pool, &e1, &batch(name, 2, 30, 40), "poll", None, rd)
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (180, 300));
        pool.close().await;
    }

    #[tokio::test]
    async fn barrier_final_ingest_after_interleaved_poll_no_double_count() {
        // F1 回归：屏障 phase A 抓取 C1 后、Manager 迟迟未 ingest final 期间，例行 poll 先读到更大的 C2
        // 并推进 baseline。随后 final(C1<C2) 到达——必须记 0，绝不把整条累计当复位重复计费。
        let (pool, e1, packed) = setup().await;
        let (name, uid) = packed.split_once('\x00').unwrap();
        let rd = reset_day(&pool).await.unwrap();
        let period = period_for(1_700_000_000, rd, ResetCycle::Monthly);

        // 基线 poll：cur=100/200 → usage 100/200, last=100/200。
        ingest_batch(&pool, &e1, &batch(name, 1, 100, 200), "poll", None, rd)
            .await
            .unwrap();
        // phase A 抓取 final C1=120/220（boot=1, seq=1），此刻尚未 ingest。
        let mut c1 = batch(name, 1, 120, 220);
        c1.sequence = 1;
        // 交错的例行 poll 先读 C2=150/260：delta 50/60 → usage 150/260，last 推到 150/260。
        ingest_batch(&pool, &e1, &batch(name, 1, 150, 260), "poll", None, rd)
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (150, 260));
        // 现在 Manager 才 ingest final C1（<last）：delta=max(0,120-150)=0 → 用量不变。
        ingest_batch(&pool, &e1, &c1, "final", None, rd)
            .await
            .unwrap();
        assert_eq!(
            period_usage(&pool, uid, &period).await.unwrap(),
            (150, 260),
            "F1：滞后 final 不得重复计费"
        );
        pool.close().await;
    }

    #[tokio::test]
    async fn final_batch_deduped() {
        let (pool, e1, packed) = setup().await;
        let (name, uid) = packed.split_once('\x00').unwrap();
        let rd = reset_day(&pool).await.unwrap();
        let mut b = batch(name, 1, 500, 500);
        b.sequence = 7;
        ingest_batch(&pool, &e1, &b, "final", None, rd)
            .await
            .unwrap();
        // 重投同 (entry,boot,seq) → 台账 UNIQUE 拦截，不双计。
        ingest_batch(&pool, &e1, &b, "final", None, rd)
            .await
            .unwrap();
        let period = period_for(1_700_000_000, rd, ResetCycle::Monthly);
        assert_eq!(period_usage(&pool, uid, &period).await.unwrap(), (500, 500));
        pool.close().await;
    }
}

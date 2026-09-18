//! 结算测试的脚手架：一个真库、一个已批准的 runtime、一个可改的快照。
//!
//! 快照从 M0 的上游样例改出来，而不是在这里手捏一份——
//! 手捏的那份不会因为上游改形状而变红（`docs/integration-contract.md` §2.3）。

#![allow(dead_code)]

use proxy_manager::config::StorageConfig;
use proxy_manager::ledger::runtime;
use proxy_manager::ledger::settle::{self, FirstSnapshot, Receipt, SettleOutcome};
use proxy_manager::store::Store;
use sqlx::Row;
use time::OffsetDateTime;

use super::fixtures;

pub const NODE: &str = "node-example-01";
pub const RUNTIME: &str = "0123456789abcdef0123456789abcdef";
pub const STARTED_AT_MS: u64 = 1_787_587_200_000;
pub const SKEW: i64 = 120;

pub struct Ledger {
    pub store: Store,
    _dir: tempfile::TempDir,
}

impl Ledger {
    pub async fn new() -> Self {
        let dir = tempfile::tempdir().expect("临时目录");
        let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
        let store = Store::open(&config).await.expect("打开数据库");
        store.init_schema().await.expect("建库");

        let mut txn = store.begin_immediate().await.unwrap();
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', 'node.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
        )
        .bind(NODE)
        .bind("0".repeat(64))
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
        txn.commit().await.unwrap();

        Ledger { store, _dir: dir }
    }

    /// 登记并批准 runtime。**未登记的 runtime 不可入账**，所以大多数用例都要先走这一步。
    pub async fn approve(&self, policy: FirstSnapshot) {
        self.approve_runtime(RUNTIME, policy).await;
    }

    pub async fn approve_runtime(&self, runtime_id: &str, policy: FirstSnapshot) {
        runtime::approve(
            &self.store,
            NODE,
            runtime_id,
            STARTED_AT_MS,
            policy,
            "用例：已确认是预期的进程窗口",
            "test",
        )
        .await
        .expect("批准 runtime");
    }

    pub async fn receive(&self, snapshot: &Snapshot) -> Option<Receipt> {
        let now = OffsetDateTime::now_utc();
        settle::receive(&self.store, NODE, &snapshot.bytes(), now, now, SKEW)
            .await
            .expect("接收不该失败")
    }

    pub async fn receive_at(
        &self,
        snapshot: &Snapshot,
        collected_at: OffsetDateTime,
        received_at: OffsetDateTime,
    ) -> Option<Receipt> {
        settle::receive(&self.store, NODE, &snapshot.bytes(), collected_at, received_at, SKEW)
            .await
            .expect("接收不该失败")
    }

    pub async fn settle(&self) -> SettleOutcome {
        self.settle_runtime(RUNTIME).await
    }

    pub async fn settle_runtime(&self, runtime_id: &str) -> SettleOutcome {
        settle::settle_next(&self.store, NODE, runtime_id).await.expect("结算不该返回 Err")
    }

    /// 接收 + 结算一次。
    pub async fn ingest(&self, snapshot: &Snapshot) -> SettleOutcome {
        self.receive(snapshot).await;
        self.settle().await
    }

    // ---- 观察点 ----

    pub async fn ledger_rows(&self) -> i64 {
        self.scalar("SELECT count(*) FROM usage_ledger").await
    }

    pub async fn cursor_count(&self) -> i64 {
        self.scalar("SELECT count(*) FROM counter_cursors").await
    }

    /// 某个身份的游标四向绝对值。
    pub async fn cursor(&self, identity: &str) -> Option<[u64; 4]> {
        let row = sqlx::query(
            "SELECT c.tcp_uplink_bytes, c.tcp_downlink_bytes, c.udp_uplink_bytes, c.udp_downlink_bytes \
             FROM counter_cursors c JOIN runtime_identities i \
                  ON i.runtime_identity_id = c.runtime_identity_id \
             WHERE i.identity_name = ?",
        )
        .bind(identity)
        .fetch_optional(self.store.readers())
        .await
        .unwrap()?;
        let mut out = [0u64; 4];
        for (index, value) in out.iter_mut().enumerate() {
            *value = row.get::<String, _>(index).trim_start_matches('0').parse().unwrap_or(0);
        }
        Some(out)
    }

    /// 某个身份的永久累计四向之和。
    pub async fn lifetime_total(&self, identity: &str) -> u128 {
        let row = sqlx::query(
            "SELECT l.tcp_uplink_bytes, l.tcp_downlink_bytes, l.udp_uplink_bytes, l.udp_downlink_bytes \
             FROM usage_lifetime_totals l JOIN runtime_identities i \
                  ON i.runtime_identity_id = l.runtime_identity_id \
             WHERE i.identity_name = ?",
        )
        .bind(identity)
        .fetch_optional(self.store.readers())
        .await
        .unwrap();
        let Some(row) = row else { return 0 };
        (0..4)
            .map(|index| {
                row.get::<String, _>(index).trim_start_matches('0').parse::<u128>().unwrap_or(0)
            })
            .sum()
    }

    pub async fn last_sequence(&self) -> Option<u64> {
        runtime::load(&self.store, NODE, RUNTIME).await.unwrap().and_then(|w| w.last_sequence)
    }

    pub async fn batch_status(&self, batch_pk: i64) -> String {
        sqlx::query("SELECT status FROM snapshot_batches WHERE batch_pk = ?")
            .bind(batch_pk)
            .fetch_one(self.store.readers())
            .await
            .unwrap()
            .get(0)
    }

    pub async fn rollup_version(&self, hour_bucket: &str) -> Option<i64> {
        sqlx::query("SELECT version FROM rollup_jobs WHERE hour_bucket = ?")
            .bind(hour_bucket)
            .fetch_optional(self.store.readers())
            .await
            .unwrap()
            .map(|row| row.get(0))
    }

    async fn scalar(&self, sql: &str) -> i64 {
        sqlx::query(sql).fetch_one(self.store.readers()).await.unwrap().get(0)
    }
}

/// 可改的快照。基线来自上游样例。
#[derive(Clone)]
pub struct Snapshot {
    value: serde_json::Value,
}

impl Default for Snapshot {
    fn default() -> Self {
        Snapshot::new()
    }
}

impl Snapshot {
    pub fn new() -> Self {
        Snapshot { value: fixtures::base_snapshot() }
    }

    pub fn sequence(mut self, sequence: u64) -> Self {
        self.value["sequence"] = serde_json::json!(sequence);
        self
    }

    pub fn runtime(mut self, runtime_id: &str) -> Self {
        self.value["runtime_id"] = serde_json::json!(runtime_id);
        self
    }

    pub fn health(mut self, key: &str, value: bool) -> Self {
        self.value["health"][key] = serde_json::json!(value);
        self
    }

    pub fn counter(mut self, identity: &str, field: &str, value: u64) -> Self {
        for inbound in self.value["inbounds"].as_array_mut().unwrap() {
            for user in inbound["users"].as_array_mut().unwrap() {
                if user["name"].as_str() == Some(identity) {
                    user[field] = serde_json::json!(value);
                    return self;
                }
            }
        }
        panic!("样例里没有身份 {identity}");
    }

    pub fn active(mut self, identity: &str, active: bool) -> Self {
        for inbound in self.value["inbounds"].as_array_mut().unwrap() {
            for user in inbound["users"].as_array_mut().unwrap() {
                if user["name"].as_str() == Some(identity) {
                    user["active"] = serde_json::json!(active);
                    return self;
                }
            }
        }
        panic!("样例里没有身份 {identity}");
    }

    pub fn drop_identity(mut self, identity: &str) -> Self {
        for inbound in self.value["inbounds"].as_array_mut().unwrap() {
            inbound["users"]
                .as_array_mut()
                .unwrap()
                .retain(|user| user["name"].as_str() != Some(identity));
        }
        self
    }

    pub fn bytes(&self) -> Vec<u8> {
        let mut out = serde_json::to_vec(&self.value).unwrap();
        out.push(b'\n');
        out
    }
}

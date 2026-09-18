//! 下发状态机的全栈脚手架：真 agent + 假节点 + 真库。
//!
//! 假节点在 M0 就实现了配额端点并**向测试暴露当前生效的配额表**，
//! 所以这一轮的断言可以落在生效表上而不是状态码上——
//! `PUT` 返回 200 什么都证明不了，空表同样返回 200。

#![allow(dead_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use proxy_manager::agent::AgentClient;
use proxy_manager::config::StorageConfig;
use proxy_manager::ledger::runtime;
use proxy_manager::ledger::settle::{self, FirstSnapshot};
use proxy_manager::quota::dispatch::{self, Prepared, PushOutcome};
use proxy_manager::quota::table::{self, NodeIdentity};
use proxy_manager::quota::wire::Quota;
use proxy_manager::store::Store;
use proxy_manager_agent::{commands::Executor, config::AgentConfig, server::Server};

use super::fake_node::FakeNode;
use super::harness::{self, DNS_NAME};

pub const NODE: &str = "node-example-01";
pub const RUNTIME: &str = "0123456789abcdef0123456789abcdef";
pub const STARTED_AT_MS: u64 = 1_787_587_200_000;

pub struct Stack {
    _materials: harness::Materials,
    _dir: tempfile::TempDir,
    pub node: FakeNode,
    pub store: Arc<Store>,
    pub client: AgentClient,
}

impl Stack {
    /// 起一条完整链路，并把首快照结算掉——这样库里才有身份可枚举。
    pub async fn new() -> Self {
        let materials = harness::materials();
        let node = FakeNode::start().await;
        let dir = tempfile::tempdir().unwrap();

        let config_toml = format!(
            "node_id = \"{NODE}\"\nlisten = \"127.0.0.1:0\"\nmaterials_dir = \"{}\"\n\n\
             [sockets]\nsnapshot = \"{}\"\nquota = \"{}\"\n",
            materials.agent_dir.display(),
            node.snapshot_socket.display(),
            node.quota_socket.display(),
        );
        let config_path = dir.path().join("agent.toml");
        std::fs::write(&config_path, config_toml).unwrap();
        let agent_config = AgentConfig::load(&config_path).unwrap();
        let server_config = proxy_manager_wire::tls::server_config(&materials.agent_dir).unwrap();
        let hmac =
            proxy_manager_wire::tls::load_hmac_key(&materials.agent_dir.join("hmac.key")).unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = listener.local_addr().unwrap().to_string();
        tokio::spawn(Server::new(server_config, Executor::new(agent_config), hmac).serve(listener));

        let storage = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
        let store = Store::open(&storage).await.unwrap();
        store.init_schema().await.unwrap();
        // 单行设置由启动流程建出来（apply 只负责改，不负责建），测试装置照做。
        proxy_manager::quota::settings::ensure_initialized(&store, "+08:00").await.unwrap();

        let mut txn = store.begin_immediate().await.unwrap();
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', 'node-01.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
        )
        .bind(NODE)
        .bind("0".repeat(64))
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
        txn.commit().await.unwrap();

        let client =
            AgentClient::new(NODE, &endpoint, DNS_NAME, &materials.controller_dir).unwrap();
        let stack =
            Stack { _materials: materials, _dir: dir, node, store: Arc::new(store), client };
        stack.approve(RUNTIME).await;
        stack.settle_once().await;
        stack
    }

    pub async fn approve(&self, runtime_id: &str) {
        runtime::approve(
            &self.store,
            NODE,
            runtime_id,
            STARTED_AT_MS,
            FirstSnapshot::Baseline,
            "用例",
            "test",
        )
        .await
        .unwrap();
    }

    /// 经 agent 取一次快照并结算，让库里有身份。
    pub async fn settle_once(&self) -> u64 {
        let response = self.client.snapshot().await.expect("采集");
        let now = time::OffsetDateTime::now_utc();
        let receipt = settle::receive(&self.store, NODE, &response.body, now, now, 120)
            .await
            .unwrap()
            .expect("回执");
        loop {
            match settle::settle_next(&self.store, NODE, &receipt.runtime_id).await.unwrap() {
                proxy_manager::ledger::SettleOutcome::Nothing => break,
                _ => continue,
            }
        }
        receipt.sequence
    }

    pub async fn identities(&self) -> Vec<NodeIdentity> {
        table::current_identities(&self.store, NODE, RUNTIME).await.unwrap()
    }

    /// 把每个身份都映射到一个用户，便于验 C30 的全量集合。
    pub async fn map_users(&self) -> BTreeMap<String, i64> {
        let identities = self.identities().await;
        let mut txn = self.store.begin_immediate().await.unwrap();
        let mut mapping = BTreeMap::new();
        for (index, identity) in identities.iter().enumerate() {
            let login = format!("u{index}");
            let row = sqlx::query(
                "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
                 VALUES (?, ?, 'user', 'normal', 'active', ?, ?) RETURNING user_id",
            )
            .bind(&login)
            .bind(&login)
            .bind("2026-09-18T00:00:00Z")
            .bind("2026-09-18T00:00:00Z")
            .fetch_one(txn.conn())
            .await
            .unwrap();
            use sqlx::Row;
            let user_id: i64 = row.get(0);
            sqlx::query("UPDATE runtime_identities SET user_id = ? WHERE runtime_identity_id = ?")
                .bind(user_id)
                .bind(identity.runtime_identity_id)
                .execute(txn.conn())
                .await
                .unwrap();
            mapping.insert(identity.name.clone(), user_id);
        }
        txn.commit().await.unwrap();
        mapping
    }

    /// 准备并发送一份表，返回 (已持久化的请求, 推送结果)。
    pub async fn push(
        &self,
        entries: Vec<(String, String, Quota)>,
        source_sequence: Option<u64>,
    ) -> (Prepared, PushOutcome) {
        let prepared =
            dispatch::prepare(&self.store, NODE, RUNTIME, entries, source_sequence, 1, 1)
                .await
                .expect("准备请求");
        let outcome = dispatch::send(&self.client, &prepared).await;
        dispatch::record(&self.store, &prepared, &outcome).await.unwrap();
        (prepared, outcome)
    }

    /// 生效表里被限额的身份集合。
    pub async fn limited_set(&self) -> BTreeSet<(String, String)> {
        self.node
            .effective_quota()
            .await
            .into_iter()
            .filter(|(_, q)| matches!(q, Quota::Limited(_)))
            .map(|(k, _)| k)
            .collect()
    }

    pub async fn effective(&self) -> BTreeMap<(String, String), Quota> {
        self.node.effective_quota().await
    }

    pub async fn request_result(&self, request_id: i64) -> String {
        use sqlx::Row;
        sqlx::query("SELECT result FROM quota_requests WHERE request_id = ?")
            .bind(request_id)
            .fetch_one(self.store.readers())
            .await
            .unwrap()
            .get(0)
    }

    pub async fn conflict_cause(&self, request_id: i64) -> Option<String> {
        use sqlx::Row;
        sqlx::query("SELECT conflict_cause FROM quota_requests WHERE request_id = ?")
            .bind(request_id)
            .fetch_one(self.store.readers())
            .await
            .unwrap()
            .get(0)
    }

    /// 取快照读回 `(node_id, runtime_id)`——409 判别的第二、三步要用。
    pub async fn observe(&self) -> Option<(String, String)> {
        let response = self.client.snapshot().await.ok()?;
        let parsed = proxy_manager::collect::snapshot::parse(&response.body).ok()?;
        Some((parsed.node_id, parsed.runtime_id))
    }
}

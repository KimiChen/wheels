//! M4 测试共用的 router 驱动器。
//!
//! 用 `tower::oneshot` 直接驱动 router：不起网络、不开端口，
//! 但走的是**真实的提取器、中间件与 handler**，不是在测试里重写一遍分发。
//!
//! 页面用例与 API 用例共用同一个 `Api`：会话是怎么建的、cookie 是怎么带的，
//! 两边必须完全一致——否则「API 拦住了」不能推出「页面也拦住了」。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use http_body_util::BodyExt;
use proxy_manager::api::session::{self, Role};
use proxy_manager::config::StorageConfig;
use proxy_manager::store::Store;
use serde_json::Value;
use tower::ServiceExt;

pub struct Api {
    pub _dir: tempfile::TempDir,
    pub store: Arc<Store>,
    pub router: axum::Router,
}

pub struct Actor {
    pub user_id: i64,
    pub session_pk: i64,
    pub cookie: String,
    pub csrf: String,
}

impl Api {
    pub async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
        let store = Arc::new(Store::open(&config).await.unwrap());
        store.init_schema().await.unwrap();
        let router = proxy_manager::api::router(store.clone());
        Api { _dir: dir, store, router }
    }

    pub async fn user(&self, login: &str, role: Role, provider: &str) -> Actor {
        use sqlx::Row;
        let mut txn = self.store.begin_immediate().await.unwrap();
        let row = sqlx::query(
            "INSERT INTO users(login_name, display_name, role, status, created_at, updated_at) \
             VALUES (?, ?, ?, 'active', ?, ?) RETURNING user_id",
        )
        .bind(login)
        .bind(login)
        .bind(role.as_str())
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .fetch_one(txn.conn())
        .await
        .unwrap();
        let user_id: i64 = row.get(0);
        txn.commit().await.unwrap();

        let issued = session::create(&self.store, user_id, provider, time::Duration::hours(1))
            .await
            .unwrap();
        Actor {
            user_id,
            session_pk: issued.session_pk,
            cookie: issued.cookie_value,
            csrf: issued.csrf_token,
        }
    }

    pub async fn admin(&self) -> Actor {
        self.user("admin", Role::Admin, "local").await
    }

    pub async fn plain_user(&self) -> Actor {
        self.user("plain", Role::User, "local").await
    }

    pub async fn get(
        &self,
        path: &str,
        actor: Option<&Actor>,
    ) -> (StatusCode, Value, axum::http::HeaderMap) {
        let mut builder = Request::builder().uri(path).method("GET");
        if let Some(actor) = actor {
            builder = builder
                .header(header::COOKIE, format!("{}={}", session::SESSION_COOKIE, actor.cookie));
        }
        self.send(builder.body(Body::empty()).unwrap()).await
    }

    pub async fn put_json(
        &self,
        path: &str,
        actor: &Actor,
        body: Value,
        csrf: Option<&str>,
        if_match: Option<&str>,
    ) -> (StatusCode, Value, axum::http::HeaderMap) {
        let mut builder = Request::builder().uri(path).method("PUT").header(
            header::COOKIE,
            format!(
                "{}={}; {}={}",
                session::SESSION_COOKIE,
                actor.cookie,
                session::CSRF_COOKIE,
                actor.csrf
            ),
        );
        if let Some(token) = csrf {
            builder = builder.header(session::CSRF_HEADER, token);
        }
        if let Some(revision) = if_match {
            builder = builder.header(header::IF_MATCH, revision);
        }
        builder = builder.header(header::CONTENT_TYPE, "application/json");
        self.send(builder.body(Body::from(body.to_string())).unwrap()).await
    }

    pub async fn send(&self, request: Request<Body>) -> (StatusCode, Value, axum::http::HeaderMap) {
        let response = self.router.clone().oneshot(request).await.unwrap();
        let status = response.status();
        let headers = response.headers().clone();
        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
        (status, value, headers)
    }
}

impl Api {
    /// 取页面/资源的**原文**，不做 JSON 解析。
    pub async fn get_text(
        &self,
        path: &str,
        cookie: Option<&str>,
    ) -> (StatusCode, String, axum::http::HeaderMap) {
        let mut builder = Request::builder().uri(path).method("GET");
        if let Some(raw) = cookie {
            builder =
                builder.header(header::COOKIE, format!("{}={}", session::SESSION_COOKIE, raw));
        }
        let response =
            self.router.clone().oneshot(builder.body(Body::empty()).unwrap()).await.unwrap();
        let status = response.status();
        let headers = response.headers().clone();
        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        (status, String::from_utf8_lossy(&bytes).into_owned(), headers)
    }
}

/// 让账本行由**真实结算**产生。
///
/// 直接 `INSERT INTO usage_ledger` 会让用例用上与被测代码同一个格式化函数，
/// 于是「结算写的键」和「查询找的键」不一致这件事永远测不出来。
/// 这里走 `runtime::approve` → `settle::receive` → `settle::settle_next`，
/// 与生产路径完全一致，只是把 `collected_at` 拨到指定的小时。
impl Api {
    pub async fn seed_ledger(&self, node_id: &str, points: &[(i64, u64)]) {
        use proxy_manager::ledger::runtime;
        use proxy_manager::ledger::settle::{self, FirstSnapshot};

        const RUNTIME: &str = "a1b2c3d4e5f60718293a4b5c6d7e8f90";
        let mut txn = self.store.begin_immediate().await.unwrap();
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', '203.0.113.9:8443', 'baseline', ?, 1, 'active', ?, ?)",
        )
        .bind(node_id)
        .bind("0".repeat(64))
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
        txn.commit().await.unwrap();

        runtime::approve(
            &self.store,
            node_id,
            RUNTIME,
            1_700_000_000_000,
            FirstSnapshot::Baseline,
            "用例：已确认是预期的进程窗口",
            "test",
        )
        .await
        .expect("批准 runtime");

        // baseline 策略：首快照只建基线，所以计数器要从 0 起一路累加，
        // 之后每一发的**增量**才是该小时的流量。
        let skew = 5 * 60 * 1000; // 允许的时钟偏差（毫秒）
        let mut cumulative = 0_u64;
        let base = crate::ledger_harness::Snapshot::new().runtime(RUNTIME);
        let schedule = [&[(0_i64, 0_u64)][..], points].concat();
        for (sequence, (hours_ago, delta)) in (1_u64..).zip(schedule) {
            cumulative += delta;
            let at = proxy_manager::ledger::bucket::hour_bucket(time::OffsetDateTime::now_utc())
                - time::Duration::hours(hours_ago)
                + time::Duration::minutes(30);
            let snapshot = base.clone().sequence(sequence).counter(
                "u_example_01",
                "tcp_uplink_bytes",
                cumulative,
            );
            settle::receive(&self.store, node_id, &snapshot.bytes(), at, at, skew)
                .await
                .expect("接收不该失败");
            settle::settle_next(&self.store, node_id, RUNTIME).await.expect("结算不该 Err");
        }
    }
}

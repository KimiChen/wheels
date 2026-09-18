//! M4 测试共用的 router 驱动器。
//!
//! 用 `tower::oneshot` 直接驱动 router：不起网络、不开端口，
//! 但走的是**真实的提取器、中间件与 handler**，不是在测试里重写一遍分发。
//!
//! 页面用例与 API 用例共用同一个 `Api`：会话是怎么建的、cookie 是怎么带的，
//! 两边必须完全一致——否则「API 拦住了」不能推出「页面也拦住了」。
//!
//! M5 也用它（登录路由要经同一套提取器与中间件），所以每个测试二进制
//! 各自只用到一部分方法——没用到的那些在另一个二进制里是活的。

#![allow(dead_code)]

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
    pub _dir: Option<tempfile::TempDir>,
    pub store: Arc<Store>,
    pub router: axum::Router,
    /// 名单文件的位置。`None` 表示这份 Api 没有 `[sso]`。
    pub sso_path: Option<std::path::PathBuf>,
}

pub struct Actor {
    pub user_id: i64,
    pub session_pk: i64,
    pub cookie: String,
    pub csrf: String,
}

/// 一份只列了管理员的最小 `server.toml` 正文。
///
/// 授权从名单现解析（D27），所以用例要让谁当管理员，就得把他写进名单——
/// **不能**靠往 `users.role` 里塞一个 `admin` 了事。那正是这套设计要挡住的事：
/// 库里那一列是展示缓存，改它不改变任何人的权限。
pub fn server_toml_with(admins: &[&str], quota_groups: &[(&str, &[&str])]) -> String {
    let list = |xs: &[&str]| xs.iter().map(|a| format!("{a:?}")).collect::<Vec<_>>().join(", ");
    let mut toml = format!(
        "[listen]\napi = \"127.0.0.1:0\"\n\
         [storage]\npath = \"/tmp/pm.db\"\n\
         [collect]\ninterval_secs = 60\nmin_interval_secs = 10\nsnapshot_max_age_secs = 120\n\
         [quota]\ndisplay_timezone = \"+08:00\"\n\
         [sso]\n\
         login_url = \"https://account.example.com/sso\"\n\
         check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
         public_base_url = \"https://pm.example.com\"\n\
         admins = [{}]\n",
        list(admins)
    );
    if !quota_groups.is_empty() {
        toml.push_str("[sso.quota_groups]\n");
        for (group, members) in quota_groups {
            toml.push_str(&format!("{group} = [{}]\n", list(members)));
        }
    }
    toml
}

impl Api {
    /// 没有 `[sso]`：所有人都是普通用户，break-glass 走 `users.role`。
    pub async fn new() -> Self {
        Api::build(None).await
    }

    /// 指定管理员名单。名单**落成一个真文件**，于是用例走的是与生产
    /// 完全相同的那条路：每请求按 mtime + inode 重读。
    pub async fn with_admins(admins: &[&str]) -> Self {
        Api::with_roster(admins, &[]).await
    }

    pub async fn with_roster(admins: &[&str], groups: &[(&str, &[&str])]) -> Self {
        Api::build(Some(server_toml_with(admins, groups))).await
    }

    /// 直接给一份 `server.toml` 正文。
    pub async fn with_server_toml(text: String) -> Self {
        Api::build(Some(text)).await
    }

    /// 改名单。**同一个 router、同一个库、同一张 cookie**——
    /// 只有磁盘上那个文件变了。这是「改名单下一个请求即生效」的真实现场。
    pub fn rewrite_roster(&self, admins: &[&str], groups: &[(&str, &[&str])]) {
        let path = self.sso_path.as_ref().expect("这份 Api 没有 [sso]");
        // 睡一下再写：某些文件系统的 mtime 粒度是秒级，同秒内改写可能
        // 连 mtime 都不变。inode 与大小同理可能撞上，所以用例要确保变化可见。
        std::thread::sleep(std::time::Duration::from_millis(1100));
        std::fs::write(path, server_toml_with(admins, groups)).unwrap();
    }

    /// 把授权源弄坏——用来验「失败关闭」。
    pub fn corrupt_roster(&self) {
        let path = self.sso_path.as_ref().expect("这份 Api 没有 [sso]");
        std::thread::sleep(std::time::Duration::from_millis(1100));
        std::fs::write(path, "这不是 TOML {{{").unwrap();
    }

    async fn build(server_toml: Option<String>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
        let store = Arc::new(Store::open(&config).await.unwrap());
        store.init_schema().await.unwrap();
        // 单行设置由启动流程建出来（apply 只负责改，不负责建），测试装置照做。
        proxy_manager::quota::settings::ensure_initialized(&store, "+08:00").await.unwrap();
        let (sso, sso_path) = match server_toml {
            Some(text) => {
                let path = dir.path().join("server.toml");
                std::fs::write(&path, &text).unwrap();
                let parsed = proxy_manager::config::ServerConfig::from_str(&text, &path).unwrap();
                let settings = parsed.sso.expect("用例的 server.toml 必须有 [sso]");
                (
                    Some(proxy_manager::sso::Sso::new_with_source(&settings, &path).unwrap()),
                    Some(path),
                )
            }
            None => (None, None),
        };
        let router = proxy_manager::api::router(store.clone(), Arc::new(Vec::new()), sso);
        Api { _dir: Some(dir), store, router, sso_path }
    }

    pub async fn user(&self, login: &str, role: Role, provider: &str) -> Actor {
        use sqlx::Row;
        let mut txn = self.store.begin_immediate().await.unwrap();
        let row = sqlx::query(
            "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
             VALUES (?, ?, ?, 'normal', 'active', ?, ?) RETURNING user_id",
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

    /// `POST` 一个 JSON 体，带齐两个 cookie 与 CSRF header。
    pub async fn post(
        &self,
        path: &str,
        actor: &Actor,
        body: Value,
    ) -> (StatusCode, Value, axum::http::HeaderMap) {
        let builder = Request::builder()
            .uri(path)
            .method("POST")
            .header(
                header::COOKIE,
                format!(
                    "{}={}; {}={}",
                    session::SESSION_COOKIE,
                    actor.cookie,
                    session::CSRF_COOKIE,
                    actor.csrf
                ),
            )
            .header(session::CSRF_HEADER, &actor.csrf)
            .header(header::CONTENT_TYPE, "application/json");
        self.send(builder.body(Body::from(body.to_string())).unwrap()).await
    }

    /// 同上但**不带** CSRF header：写操作的反向用例要用。
    pub async fn post_without_csrf(
        &self,
        path: &str,
        actor: &Actor,
    ) -> (StatusCode, Value, axum::http::HeaderMap) {
        let builder = Request::builder().uri(path).method("POST").header(
            header::COOKIE,
            format!(
                "{}={}; {}={}",
                session::SESSION_COOKIE,
                actor.cookie,
                session::CSRF_COOKIE,
                actor.csrf
            ),
        );
        self.send(builder.body(Body::empty()).unwrap()).await
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
    /// 带一整条自定义 `Cookie` 头取原文——回调用例要送 state cookie。
    pub async fn get_text_with_cookie(
        &self,
        path: &str,
        cookie_header: &str,
    ) -> (StatusCode, String, axum::http::HeaderMap) {
        self.get_text_raw(path, Some(cookie_header.to_string())).await
    }

    /// 取页面/资源的**原文**，不做 JSON 解析。
    pub async fn get_text(
        &self,
        path: &str,
        cookie: Option<&str>,
    ) -> (StatusCode, String, axum::http::HeaderMap) {
        self.get_text_raw(path, cookie.map(|raw| format!("{}={}", session::SESSION_COOKIE, raw)))
            .await
    }

    async fn get_text_raw(
        &self,
        path: &str,
        cookie_header: Option<String>,
    ) -> (StatusCode, String, axum::http::HeaderMap) {
        let mut builder = Request::builder().uri(path).method("GET");
        if let Some(raw) = cookie_header {
            builder = builder.header(header::COOKIE, raw);
        }
        let response =
            self.router.clone().oneshot(builder.body(Body::empty()).unwrap()).await.unwrap();
        let status = response.status();
        let headers = response.headers().clone();
        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        (status, String::from_utf8_lossy(&bytes).into_owned(), headers)
    }
}

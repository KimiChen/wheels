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
    /// 审计镜像树的根。用例往里铺 JSONL 夹具。
    pub audit_dir: std::path::PathBuf,
    /// 凭据文件的位置。`None` 表示这份 Api 没有 `[subscription]`。
    pub credentials_path: Option<std::path::PathBuf>,
}

impl Api {
    /// 原地改写凭据文件。
    ///
    /// 用来模拟**推送顺序**造成的中间态：二进制推了、凭据还没推。
    /// 文件是按 mtime 重读的（凭据不进数据库），所以改完下一次请求就生效。
    pub fn rewrite_credentials(&self, edit: impl FnOnce(&mut String)) {
        let path = self.credentials_path.as_ref().expect("这份 Api 没有 [subscription]");
        let mut text = std::fs::read_to_string(path).unwrap();
        edit(&mut text);
        std::fs::write(path, text).unwrap();
        std::fs::set_permissions(path, std::os::unix::fs::PermissionsExt::from_mode(0o600))
            .unwrap();
        // 不用碰 mtime：`CredentialSource` 的缓存键是 inode + **长度** + mtime，
        // 改写只要改了长度就一定会重读。靠 mtime 的话得处理它只精确到秒这件事。
    }
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

    /// 带订阅的 Api：写一份凭据文件（0600）与入口清单。
    pub async fn with_subscription(admins: &[&str], identities: &[&str]) -> Self {
        Api::build_full(Some(server_toml_with(admins, &[])), Some(identities.to_vec()), true).await
    }

    async fn build(server_toml: Option<String>) -> Self {
        Api::build_full(server_toml, None, true).await
    }

    /// 没有 `[audit]` 的一份。审计端点那时回 `audit_not_enabled` 而不是空表——
    /// 空表会被读成「你没访问过任何目标」。
    pub async fn without_audit() -> Self {
        Api::build_full(None, None, false).await
    }

    async fn build_full(
        server_toml: Option<String>,
        subscription_identities: Option<Vec<&str>>,
        with_audit: bool,
    ) -> Self {
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
        // 订阅：凭据文件必须 0600，否则 CredentialSource 会拒绝读它。
        let subscription = subscription_identities.map(|identities| {
            let creds = dir.path().join("credentials.toml");
            let mut text = String::from(
                "method = \"2022-blake3-aes-128-gcm\"\nipsk = \"dGVzdC1pcHNr\"\n[upsk]\n",
            );
            for (index, name) in identities.iter().enumerate() {
                text.push_str(
                    &format!("{name} = \"dXBzay17aW5kZXh9\"\n")
                        .replace("{index}", &index.to_string()),
                );
            }
            // uuid 与 upsk 的身份集合必须**完全一致**：`Credentials::validate` 拒绝
            // 「有一半」的文件，因为那种文件的症状是一部分人的公网订阅悄悄没有节点。
            text.push_str("[uuid]\n");
            for (index, name) in identities.iter().enumerate() {
                text.push_str(&format!("{name} = \"00000000-0000-4000-8000-{:012}\"\n", index + 1));
            }
            std::fs::write(&creds, text).unwrap();
            std::fs::set_permissions(&creds, std::os::unix::fs::PermissionsExt::from_mode(0o600))
                .unwrap();
            proxy_manager::config::SubscriptionConfig {
                public_base_url: "https://pm.example.com".into(),
                credentials_path: creds,
                // 两种到达方式，共用同一批端口与凭据——与生产同形。
                sources: vec![
                    proxy_manager::config::EntrySource {
                        prefix: "proxyWan-".into(),
                        host: "203.0.113.1".into(),
                        note: "公网".into(),
                        transport: proxy_manager::config::Transport::Vless,
                        min_level: 0,
                    },
                    proxy_manager::config::EntrySource {
                        prefix: "proxyLan-".into(),
                        host: "198.51.100.1".into(),
                        note: "内网".into(),
                        transport: proxy_manager::config::Transport::Ss,
                        min_level: 0,
                    },
                ],
                vless: Some(proxy_manager::config::VlessConfig {
                    servername: "www.example.com".into(),
                    client_fingerprint: "chrome".into(),
                    flow: "xtls-rprx-vision".into(),
                    packet_encoding: "xudp".into(),
                    reality: [(
                        "node-a".to_string(),
                        proxy_manager::config::Reality {
                            public_key: "cHVibGljLWtleS1mb3ItdGVzdHMtMDAwMDAwMDAwMDA".into(),
                            short_id: "01234567".into(),
                        },
                    )]
                    .into_iter()
                    .collect(),
                }),
                groups: vec![proxy_manager::config::ProxyGroup {
                    name: "手动选择".into(),
                    icon: None,
                    proxies: vec!["HK".into(), "JP".into(), "DIRECT".into()],
                }],
                entries: vec![
                    proxy_manager::config::Entry {
                        name: "HK".into(),
                        host: None,
                        ports: [("ss".to_string(), 65002), ("vless".to_string(), 65081)]
                            .into_iter()
                            .collect(),
                        node_id: "node-a".into(),
                    },
                    proxy_manager::config::Entry {
                        name: "JP".into(),
                        host: None,
                        ports: [("ss".to_string(), 65003), ("vless".to_string(), 65082)]
                            .into_iter()
                            .collect(),
                        node_id: "node-a".into(),
                    },
                ],
            }
        });
        // 审计镜像树放在这个用例自己的临时目录下，与真实部署同形。
        let audit_dir = dir.path().join("audit");
        let audit = with_audit.then(|| proxy_manager::config::AuditConfig {
            dir: audit_dir.clone(),
            interval_secs: 600,
        });
        let credentials_path = subscription.as_ref().map(|c| c.credentials_path.clone());
        let router = proxy_manager::api::router(
            store.clone(),
            Arc::new(Vec::new()),
            sso,
            subscription,
            audit,
        );
        Api { _dir: Some(dir), store, router, sso_path, audit_dir, credentials_path }
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

    /// 往审计镜像树里铺一份 JSONL。`lines` 是**整行**，照节点真实写法。
    pub fn seed_audit(&self, node_id: &str, identity: &str, lines: &[String]) {
        let dir = self.audit_dir.join(node_id);
        std::fs::create_dir_all(&dir).unwrap();
        let name = proxy_manager_wire::audit::active_file_name(identity);
        let mut body = lines.join("\n");
        body.push('\n');
        std::fs::write(dir.join(name), body).unwrap();
    }

    pub fn audit_queries(&self) -> Vec<proxy_manager::audit::queries::QueryRecord> {
        proxy_manager::audit::queries::read(&self.audit_dir).unwrap()
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

    /// 造一个**可认领**的账号池：节点 + 已批准 runtime + 身份 + 零基线游标 + free 槽位。
    ///
    /// 手工造而不是跑真结算：这一组用例验的是「登录会不会自动领」，
    /// 而不是「结算会不会登记槽位」（那条在 m2 的 identity_pool 里）。
    /// 但零基线游标必须真的写进去——`pick_free_slot` 的门禁查的就是它，
    /// 不写就等于把这条门禁从用例里摘掉了。
    pub async fn seed_claimable_pool(&self, nodes: &[&str], identities: &[&str]) {
        use sqlx::Row;
        const ZERO: &str = "00000000000000000000";
        let now = "2026-09-19T00:00:00Z";
        let mut txn = self.store.begin_immediate().await.unwrap();
        for (index, node) in nodes.iter().enumerate() {
            sqlx::query(
                "INSERT INTO nodes(node_id, provider, address, first_snapshot, \
                 agent_spki_sha256, quota_enabled, status, created_at, updated_at) \
                 VALUES (?, 'plus_v3', ?, 'baseline', ?, 1, 'active', ?, ?) \
                 ON CONFLICT(node_id) DO UPDATE SET quota_enabled = 1",
            )
            .bind(node)
            .bind(format!("{node}.example.com:8443"))
            .bind("0".repeat(64))
            .bind(now)
            .bind(now)
            .execute(txn.conn())
            .await
            .unwrap();

            let runtime = format!("{:032x}", index + 1);
            let runtime_pk: i64 = sqlx::query(
                "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, \
                 first_snapshot, approval_status, window_state, first_seen_at, last_seen_at) \
                 VALUES (?, ?, ?, 'baseline', 'approved', 'open', ?, ?) RETURNING runtime_pk",
            )
            .bind(node)
            .bind(&runtime)
            .bind(format!("{:020}", 1_789_000_000_000u64))
            .bind(now)
            .bind(now)
            .fetch_one(txn.conn())
            .await
            .map(|row| row.get(0))
            .unwrap();

            let service_id: i64 = sqlx::query(
                "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, \
                 inbound_type, active, first_seen_at) \
                 VALUES (?, 'ss-entry', ?, 'shadowsocks', 1, ?) RETURNING runtime_service_id",
            )
            .bind(runtime_pk)
            .bind(format!("{:020}", 1u64))
            .bind(now)
            .fetch_one(txn.conn())
            .await
            .map(|row| row.get(0))
            .unwrap();

            for name in identities {
                let identity_id: i64 = sqlx::query(
                    "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, \
                     identity_name, generation, active, first_seen_at, last_seen_at) \
                     VALUES (?, ?, ?, ?, 1, ?, ?) RETURNING runtime_identity_id",
                )
                .bind(runtime_pk)
                .bind(service_id)
                .bind(name)
                .bind(format!("{:020}", 1u64))
                .bind(now)
                .bind(now)
                .fetch_one(txn.conn())
                .await
                .map(|row| row.get(0))
                .unwrap();

                // 零基线：四向全为定宽零串。门禁查的就是这一行。
                sqlx::query(
                    "INSERT INTO counter_cursors(runtime_identity_id, tcp_uplink_bytes, \
                     tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes, updated_at) \
                     VALUES (?, ?, ?, ?, ?, ?)",
                )
                .bind(identity_id)
                .bind(ZERO)
                .bind(ZERO)
                .bind(ZERO)
                .bind(ZERO)
                .bind(now)
                .execute(txn.conn())
                .await
                .unwrap();

                // 槽位不带 inbound：一个名字一个槽位。上面那段建 runtime_services
                // 的 'ss-entry' 要留着——那是 lineage 的真相，只有槽位上那份拷贝该走。
                sqlx::query(
                    "INSERT INTO identity_routes(node_id, identity_name, \
                     state, created_at) VALUES (?, ?, 'free', ?)",
                )
                .bind(node)
                .bind(name)
                .bind(now)
                .execute(txn.conn())
                .await
                .unwrap();
            }
        }
        txn.commit().await.unwrap();
    }

    /// 把一个槽位弄脏：给它的游标写上非零累计，于是零基线门禁不再放它过。
    pub async fn dirty_slot(&self, node: &str, identity: &str) {
        let mut txn = self.store.begin_immediate().await.unwrap();
        sqlx::query(
            "UPDATE counter_cursors SET tcp_uplink_bytes = ? WHERE runtime_identity_id IN ( \
               SELECT i.runtime_identity_id FROM runtime_identities i \
                 JOIN node_runtimes r ON r.runtime_pk = i.runtime_pk \
                WHERE r.node_id = ? AND i.identity_name = ?)",
        )
        .bind(format!("{:020}", 4096u64))
        .bind(node)
        .bind(identity)
        .execute(txn.conn())
        .await
        .unwrap();
        txn.commit().await.unwrap();
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

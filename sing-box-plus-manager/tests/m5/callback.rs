//! 登录路由：跳转、回调顺序与退出（README §4.9 加固 3）。
//!
//! 回调那条顺序是整页的核心。这一组里最重要的一条是
//! [`匿名伪造的回调不写任何东西`]——它证明「无任何写入地校验」这句话
//! 不只是注释：把墓碑写在校验之前，登录入口就是一个免认证的写放大入口。

use axum::http::StatusCode;
use proxy_manager::api::session::Role;
use proxy_manager::sso::state;

use super::harness::Api;

/// 一份指向**立刻连接失败**的地址的 SSO 配置。
///
/// `127.0.0.1:1` 上没人监听，所以任何真的走到「换取身份」那一步的用例
/// 会在几毫秒内拿到连接拒绝，而不是卡在 DNS 上。
/// 这正好把「到不到得了上游」变成一个可观测的分界线。
async fn dead_upstream() -> Api {
    let toml = super::harness::server_toml_with(&["boss"], &[]).replace(
        "https://account.example.com/api/sso/check-token",
        "https://127.0.0.1:1/api/sso/check-token",
    );
    Api::with_server_toml(toml).await
}

async fn tombstones(api: &Api) -> i64 {
    use sqlx::Row;
    sqlx::query("SELECT count(*) FROM sso_used_states")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0)
}

async fn users(api: &Api) -> i64 {
    use sqlx::Row;
    sqlx::query("SELECT count(*) FROM users").fetch_one(api.store.readers()).await.unwrap().get(0)
}

async fn sessions(api: &Api) -> i64 {
    use sqlx::Row;
    sqlx::query("SELECT count(*) FROM sessions")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0)
}

// ============ 端点是否存在 ============

/// 没配 `[sso]` 时登录入口**不挂载**。
///
/// 404 而不是一个会 500 的按钮：SSO 依赖对外域名、回调地址与出口 IP
/// 这些主控仓库之外的事实，没谈妥之前一个能点的登录按钮只会制造误报。
#[tokio::test]
async fn 没配sso时登录入口是404() {
    let api = Api::new().await;
    assert_eq!(api.get_text("/auth/sso/login", None).await.0, StatusCode::NOT_FOUND);
    assert_eq!(api.get_text(state::CALLBACK_PATH, None).await.0, StatusCode::NOT_FOUND);
}

// ============ 跳转 ============

#[tokio::test]
async fn login发302并下发回调专用cookie() {
    let api = Api::with_admins(&["boss"]).await;
    let (status, _, headers) = api.get_text("/auth/sso/login", None).await;
    assert_eq!(status, StatusCode::FOUND, "这一跳是「去别处登录」，用 302");

    let location = headers.get("location").unwrap().to_str().unwrap();
    assert!(location.starts_with("https://account.example.com/sso?url="), "实际：{location}");

    let cookie = headers.get("set-cookie").unwrap().to_str().unwrap();
    assert!(cookie.starts_with(state::STATE_COOKIE), "实际：{cookie}");
    // `__Host-` 强制 Path=/；它换来的是「兄弟子域写不了这个 cookie」，
    // 而那正是双提交赖以成立的前提。
    assert!(cookie.contains("Path=/"), "实际：{cookie}");

    // token 会出现在回调 URL 的 query 里，所以这条链路上每一步都要压住 Referer。
    assert_eq!(headers.get("referrer-policy").unwrap(), "no-referrer");
}

// ============ 回调：顺序与拒绝 ============

/// 加固 3 的第 2 步：**拒绝未知 query 参数**。
///
/// 代价要知道：泡游哪天多追加一个参数，登录会全线失败。这是刻意选的方向——
/// 多一个没预期的参数意味着协议变了，而在认证路径上「协议变了却照常放行」
/// 是更坏的那一种。
#[tokio::test]
async fn 未知参数的回调被拒() {
    let api = dead_upstream().await;
    let (status, _, headers) =
        api.get_text(&format!("{}?state=x&token=y&from=game", state::CALLBACK_PATH), None).await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    assert!(headers.get("location").unwrap().to_str().unwrap().contains("login=failed"));
    assert_eq!(tombstones(&api).await, 0);
    assert_eq!(users(&api).await, 0);
}

/// 重复键同样拒绝：`?token=a&token=b` 在不同框架里取值不同，
/// 而「取哪一个」不该由框架的实现细节决定。
#[tokio::test]
async fn 重复参数的回调被拒() {
    let api = dead_upstream().await;
    let (status, _, _) =
        api.get_text(&format!("{}?state=x&token=a&token=b", state::CALLBACK_PATH), None).await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    assert_eq!(users(&api).await, 0);
}

/// **加固 3 的那一整句：匿名伪造的回调不创建会话、不写数据库。**
///
/// 这是这一组里最重要的一条。把墓碑写在校验之前，登录入口就变成一个
/// 免认证的写放大入口：任何人构造一个回调 URL 就能让主控写一行。
#[tokio::test]
async fn 匿名伪造的回调不写任何东西() {
    let api = dead_upstream().await;
    // 没有 cookie，state 是编的。
    let (status, _, headers) = api
        .get_text(&format!("{}?state=1789.aa.bb&token=whatever", state::CALLBACK_PATH), None)
        .await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    assert!(headers.get("location").unwrap().to_str().unwrap().contains("login=failed"));
    assert_eq!(tombstones(&api).await, 0, "校验没过就不该写墓碑");
    assert_eq!(users(&api).await, 0, "校验没过就不该建号");
    assert_eq!(sessions(&api).await, 0, "校验没过就不该建会话");
}

/// 回调**无论成败**都清掉 state cookie：它只该用一次。
#[tokio::test]
async fn 回调第一件事是清掉state_cookie() {
    let api = dead_upstream().await;
    let (_, _, headers) =
        api.get_text(&format!("{}?state=1789.aa.bb&token=x", state::CALLBACK_PATH), None).await;
    let cookie = headers.get("set-cookie").unwrap().to_str().unwrap();
    assert!(cookie.starts_with(state::STATE_COOKIE), "实际：{cookie}");
    assert!(cookie.contains("Max-Age=0"), "实际：{cookie}");
}

/// 一个**签名正确**的 state 也不能让上游不可用变成一次成功登录。
///
/// 这一条把「校验通过」与「换到了身份」分开：前者过了，后者没过，
/// 结果仍然必须是零写入——墓碑在上游成功之后才写。
#[tokio::test]
async fn 签名正确但上游不可用时仍然零写入() {
    let api = dead_upstream().await;
    let key = proxy_manager::sso::state_key(&api.store).await.unwrap();
    let (issued, _) = state::issue(&key, 300, time::OffsetDateTime::now_utc());

    let (status, _, _) = api
        .get_text_with_cookie(
            &format!("{}?state={issued}&token=real-looking-token", state::CALLBACK_PATH),
            &format!("{}={issued}", state::STATE_COOKIE),
        )
        .await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    assert_eq!(tombstones(&api).await, 0, "上游没成功就不该写墓碑——否则重试永远拿不到第二次机会");
    assert_eq!(users(&api).await, 0);
    assert_eq!(sessions(&api).await, 0);
}

/// 回调响应必须带 `Referrer-Policy: no-referrer`。
///
/// **token 就在这个 URL 的 query 里。** 没有这个头，回调页加载的任何外部资源
/// 都会在 `Referer` 里收到它。主控自己的 `private_cache_headers`
/// 只加 `Cache-Control` 与 `Vary`，补不了这一条。
#[tokio::test]
async fn 回调压住referer() {
    let api = dead_upstream().await;
    let (_, _, headers) =
        api.get_text(&format!("{}?state=1789.aa.bb&token=x", state::CALLBACK_PATH), None).await;
    assert_eq!(headers.get("referrer-policy").unwrap(), "no-referrer");
}

// ============ 退出 ============

/// 退出是**写操作**，要过 CSRF 双提交——否则任何第三方页面都能把人踢下线。
#[tokio::test]
async fn 退出要过csrf并吊销会话() {
    let api = Api::with_admins(&["boss"]).await;
    let actor = api.user("boss", Role::Admin, "paoyou").await;

    // 不带 CSRF header：拒绝。
    let (status, body, _) = api.post_without_csrf("/api/v1/auth/logout", &actor).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(body["error"]["code"], "csrf_invalid");
    // 反向证据：被拒之后会话**仍然可用**，不是「顺手也退了」。
    assert_eq!(api.get("/api/v1/me", Some(&actor)).await.0, StatusCode::OK);

    let (status, _, headers) = api.post("/api/v1/auth/logout", &actor, serde_json::json!({})).await;
    assert_eq!(status, StatusCode::OK);
    let cleared: Vec<&str> =
        headers.get_all("set-cookie").iter().map(|v| v.to_str().unwrap()).collect();
    assert_eq!(cleared.len(), 2, "会话与 CSRF 两个 cookie 都要清：{cleared:?}");
    assert!(cleared.iter().all(|c| c.contains("Max-Age=0")), "{cleared:?}");

    // 吊销每请求检查，下一个请求立即生效。
    let (status, body, _) = api.get("/api/v1/me", Some(&actor)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(body["error"]["code"], "session_revoked");
}

// ============ 登录页 ============

/// 服务端把「有没有挂载 SSO」盖进登录页。
///
/// 前端自己探测不出这件事：它只能点一次然后看见 404，而那对使用者毫无信息量。
#[tokio::test]
async fn 登录页上盖着sso是否可用() {
    let off = Api::new().await;
    let (status, body, _) = off.get_text("/login.html", None).await;
    assert_eq!(status, StatusCode::OK, "登录页必须免认证可达，否则就是死循环");
    assert!(body.contains(r#"data-pm-sso="off""#), "没配 SSO 时要盖 off");

    let on = Api::with_admins(&["boss"]).await;
    let (_, body, _) = on.get_text("/login.html", None).await;
    assert!(body.contains(r#"data-pm-sso="on""#), "配了 SSO 时要盖 on");
    // 盖属性不能把原有的 class 顶掉——整页样式都挂在它上面。
    assert!(body.contains(r#"<body class="pm-login" data-pm-sso="on""#), "class 丢了");
}

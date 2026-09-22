//! 页面级越权与静态资源分发（README §4.11）。
//!
//! **越权在服务端拦住，不是把页面发过去再藏。** 前端隐藏不是安全边界：
//! 页面一旦发到浏览器，藏起来的部分在开发者工具里一览无余。
//! 这一组用例走的是真实 router 的 fallback，不是在测试里模拟一遍路由。

use axum::http::{header, StatusCode};
use proxy_manager::api::session::Role;

use crate::harness::Api;

#[tokio::test]
async fn unauthenticated_page_request_redirects_to_login() {
    let api = Api::new().await;
    for page in ["overview.html", "nodes.html", "me.html", "index.html"] {
        let (status, _, headers) = api.get_text(&format!("/{page}"), None).await;
        assert_eq!(status, StatusCode::SEE_OTHER, "{page} 未认证时应跳登录页");
        assert_eq!(headers.get(header::LOCATION).unwrap(), "/login.html");
    }
}

#[tokio::test]
async fn ordinary_user_gets_403_body_not_the_admin_page() {
    let api = Api::new().await;
    let actor = api.user("normal", Role::User, "feishu").await;

    let (status, body, _) = api.get_text("/nodes.html", Some(&actor.cookie)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    // 关键在于**发的不是那一页**：状态码对了但内容照发，等于没拦。
    assert!(!body.contains("节点清单与能力"), "403 里出现了管理页内容，等于没拦住");
    let expected = proxy_manager::web::page_body("unauthorized.html").unwrap();
    assert_eq!(body, expected, "403 的响应体应当就是越权提示页");
}

/// 服务端把**这一次求值出的角色**盖进 `<body>`。
///
/// 没有这一步，`app.js` 会在 load 时按 localStorage 里存的原型身份决定导航，
/// 把管理员从管理页弹到个人页——而且发生在任何请求回来之前。
#[tokio::test]
async fn served_page_carries_the_server_evaluated_role() {
    // 管理员身份来自名单，不是来自 users.role（D27）。
    let api = Api::with_admins(&["boss"]).await;
    let admin = api.user("boss", Role::Admin, "feishu").await;
    let user = api.user("member", Role::User, "feishu").await;

    let (status, body, _) = api.get_text("/overview.html", Some(&admin.cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        body.contains(r#"<body data-principal="admin" data-pm-mode="live""#),
        "管理员角色未盖上"
    );

    let (status, body, _) = api.get_text("/me.html", Some(&user.cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        body.contains(r#"<body data-principal="user" data-pm-mode="live""#),
        "普通用户角色未盖上"
    );
    // 页面原文里 me.html 本来就写着 user；换一页证明盖的是**会话角色**而不是原文。
    let (_, body, _) = api.get_text("/me-usage.html", Some(&admin.cookie)).await;
    assert!(
        body.contains(r#"data-principal="admin""#),
        "管理员访问共享页时，盖上的应是会话角色而不是页面原文里的默认值"
    );
}

/// 登录页与静态资源**不需要认证**——否则登录页自己就取不到样式。
#[tokio::test]
async fn login_page_and_assets_need_no_session() {
    let api = Api::new().await;
    for path in [
        "/login.html",
        "/assets/app.js",
        "/assets/api.js",
        "/assets/theme.js",
        "/assets/console.css",
        "/web-standard-kit/style.css",
        "/web-standard-kit/script.js",
    ] {
        let (status, body, _) = api.get_text(path, None).await;
        assert_eq!(status, StatusCode::OK, "{path} 应免认证可取");
        assert!(!body.is_empty(), "{path} 内容为空");
    }
}

/// 页面与资源不得被共享缓存留下。会话态内容进了中间缓存就是跨用户串号。
#[tokio::test]
async fn authenticated_pages_are_not_publicly_cacheable() {
    let api = Api::with_admins(&["boss"]).await;
    let admin = api.user("boss", Role::Admin, "feishu").await;
    let (_, _, headers) = api.get_text("/overview.html", Some(&admin.cookie)).await;
    let cache = headers.get(header::CACHE_CONTROL).map(|v| v.to_str().unwrap()).unwrap_or("");
    assert!(cache.contains("private") || cache.contains("no-store"), "缓存头是 `{cache}`");
}

#[tokio::test]
async fn unknown_page_is_404_not_a_redirect_loop() {
    let api = Api::new().await;
    let (status, _, _) = api.get_text("/nope.html", None).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "未知路径不应跳登录页——那会变成死循环");
}

/// **每个 `data-dialog-open` 都要指向同一页里真实存在的 `<dialog>`。**
///
/// 指不到的后果不是报错，是**点了什么都不发生**——kit 那一行是
/// `document.getElementById(...)?.showModal()`，问号把失败吞掉了
/// （`web-standard-kit/script.js`）。一个按了没反应的按钮比一个没有的按钮
/// 更让人困惑，而且不会有任何东西报出来。
///
/// 2026-09-22 删原型遗留的吊销对话框时差点造出这个状态：对话框在 7 个页面里，
/// 而打开它的三个按钮在 users.html，只删对话框就会留下三个哑按钮。
#[test]
fn 对话框按钮都指向存在的对话框() {
    let mut dangling = Vec::new();
    for name in proxy_manager::web::page_names() {
        let body = proxy_manager::web::page_body(name).expect("页面该在");
        let ids: Vec<&str> = body
            .match_indices("<dialog")
            .filter_map(|(at, _)| {
                let tag = &body[at..body[at..].find('>').map(|i| at + i).unwrap_or(body.len())];
                let key = tag.find("id=\"")? + 4;
                let rest = &tag[key..];
                Some(&rest[..rest.find('"')?])
            })
            .collect();
        for (at, _) in body.match_indices("data-dialog-open=\"") {
            let rest = &body[at + "data-dialog-open=\"".len()..];
            let target = &rest[..rest.find('"').expect("属性该闭合")];
            if !ids.contains(&target) {
                dangling.push(format!("{name} 里的按钮指向不存在的 #{target}"));
            }
        }
    }
    assert!(dangling.is_empty(), "{dangling:#?}");
}

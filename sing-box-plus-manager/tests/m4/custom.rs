//! 个人自定义节点的端点。
//!
//! 重点不在「能建能删」，在三件更容易出错的事：
//! **不回凭据**、**重名与 400 分得开**、**碰不到别人的那一条**。

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use serde_json::{json, Value};

use crate::harness::{Actor, Api};
use proxy_manager::api::session;
use proxy_manager::api::session::Role;

const SS_LINK: &str = "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@up.example:8388";

async fn api() -> Api {
    Api::with_subscription(&[], &["slot-01", "slot-02"]).await
}

async fn del(api: &Api, path: &str, actor: &Actor) -> (StatusCode, Value) {
    let request = Request::builder()
        .uri(path)
        .method("DELETE")
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
        .header(session::CSRF_HEADER, actor.csrf.clone())
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = api.send(request).await;
    (status, body)
}

#[tokio::test]
async fn 建一条自定义上游并列出来() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;

    let (status, body, _) = api
        .post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    assert_eq!(status, StatusCode::OK, "{body}");

    let (status, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["enabled"], true);
    let item = &body["proxies"][0];
    assert_eq!(item["name"], "家里");
    assert_eq!(item["protocol"], "ss");
    assert_eq!(item["server"], "up.example");
    assert_eq!(item["port"], 8388);
    // 页面要显示它在客户端里叫什么，否则用户在节点列表里找不到对应的那条。
    assert_eq!(item["rendered_name"], "Custom-家里");
}

/// **列表不回凭据，也不回原始分享链接。**
///
/// 链接里就带着密码。页面会被截图、会被投屏，任何一次 XSS 都能把响应
/// 里的东西一起带走——与 `/me/subscription` 不回 host 是同一条纪律。
#[tokio::test]
async fn 列表不回密码与分享链接() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    api.post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;

    let (_, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    let text = body.to_string();
    assert!(!text.contains("password"), "不该出现密码字段：{text}");
    assert!(!text.contains("share_url"), "不该回原始链接：{text}");
    assert!(!text.contains("ss://"), "更不该回链接本身：{text}");
}

/// 格式错是 400，重名是 409。**两者要分得开**——
/// 前者改这一个字段，后者要么改名要么先删掉那一条，处置完全不同。
#[tokio::test]
async fn 格式错与重名分得开() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;

    let (status, body, _) = api
        .post(
            "/api/v1/me/custom/proxies",
            &alice,
            json!({ "name": "x", "share_url": "ss://坏链接" }),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "格式错该是 400：{body}");

    api.post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    let (status, body, _) = api
        .post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    assert_eq!(status, StatusCode::CONFLICT, "重名该是 409：{body}");
}

/// 与受管入口或代理组重名同样是 409。
#[tokio::test]
async fn 不许与受管入口重名() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    // 夹具里的入口叫 HK / JP，代理组叫「手动选择」。
    for name in ["HK", "手动选择"] {
        let (status, body, _) = api
            .post(
                "/api/v1/me/custom/proxies",
                &alice,
                json!({ "name": name, "share_url": SS_LINK }),
            )
            .await;
        assert_eq!(status, StatusCode::CONFLICT, "「{name}」该被拒：{body}");
    }
}

/// **碰不到别人的那一条。** id 是自增的，猜得到；`user_id` 必须进 WHERE。
#[tokio::test]
async fn 改不到别人的条目() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let bob = api.user("bob", Role::User, "local").await;

    let (_, body, _) = api
        .post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    let id = body["id"].as_i64().expect("该返回 id");

    let (status, body, _) = api
        .put_json(
            &format!("/api/v1/me/custom/proxies/{id}"),
            &bob,
            json!({ "name": "抢过来" }),
            Some(&bob.csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "别人的条目该是 404：{body}");

    let (status, body) = del(&api, &format!("/api/v1/me/custom/proxies/{id}"), &bob).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "删也一样：{body}");

    // alice 那一条**原样还在**——这半条不能省，否则「404」也可能是因为它被删了。
    let (_, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    assert_eq!(body["proxies"][0]["name"], "家里", "{body}");
}

/// 只改名字不用重贴链接，配置原样保留。
#[tokio::test]
async fn 只改名字保留配置() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (_, body, _) = api
        .post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    let id = body["id"].as_i64().unwrap();

    let (status, body, _) = api
        .put_json(
            &format!("/api/v1/me/custom/proxies/{id}"),
            &alice,
            json!({ "name": "公司" }),
            Some(&alice.csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{body}");

    let (_, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    assert_eq!(body["proxies"][0]["name"], "公司");
    assert_eq!(body["proxies"][0]["server"], "up.example", "配置该原样保留：{body}");
}

/// SOCKS5 的前置只能选 DIRECT、受管入口或自己的上游。
#[tokio::test]
async fn socks5的前置必须可用() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;

    let (status, body, _) = api
        .post(
            "/api/v1/me/custom/socks5",
            &alice,
            json!({ "name": "落地", "server": "s.example", "port": "1080",
                    "dialer_proxy": "并不存在" }),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "{body}");

    let (status, body, _) = api
        .post(
            "/api/v1/me/custom/socks5",
            &alice,
            json!({ "name": "落地", "server": "s.example", "port": "1080",
                    "dialer_proxy": "HK" }),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "受管入口该可选：{body}");

    let (_, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    let item = &body["socks5"][0];
    assert_eq!(item["dialer_proxy"], "HK");
    assert_eq!(item["rendered_name"], "Socks5-落地");
    assert_eq!(item["dialer_group"], "Socks5-落地-先选前置节点");
    assert_eq!(item["has_password"], false);
}

/// **被引用的上游不能删。**
///
/// 允许的话那条 SOCKS5 的前置会指向一个不存在的名字，
/// 而渲染时那是「整份订阅失败」而不是「这一条不可用」。
#[tokio::test]
async fn 被引用的上游删不掉() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (_, body, _) = api
        .post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;
    let id = body["id"].as_i64().unwrap();
    api.post(
        "/api/v1/me/custom/socks5",
        &alice,
        json!({ "name": "落地", "server": "s.example", "port": "1080",
                "dialer_proxy": "家里" }),
    )
    .await;

    let (status, body) = del(&api, &format!("/api/v1/me/custom/proxies/{id}"), &alice).await;
    assert_eq!(status, StatusCode::CONFLICT, "被引用着该拒绝删除：{body}");
    // 改名同理——改了之后引用也会悬空。
    let (status, body, _) = api
        .put_json(
            &format!("/api/v1/me/custom/proxies/{id}"),
            &alice,
            json!({ "name": "换个名" }),
            Some(&alice.csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::CONFLICT, "被引用着也不该改名：{body}");
}

/// 拨号候选里要有 DIRECT、受管入口和自己的上游，**且用的是原名**。
#[tokio::test]
async fn 拨号候选一次取全() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    api.post("/api/v1/me/custom/proxies", &alice, json!({ "name": "家里", "share_url": SS_LINK }))
        .await;

    let (_, body, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    let options: Vec<&str> =
        body["dialer_options"].as_array().unwrap().iter().map(|v| v.as_str().unwrap()).collect();
    assert!(options.contains(&"DIRECT"), "{options:?}");
    assert!(options.contains(&"HK"), "{options:?}");
    // **原名**，不带 Custom- 前缀：库里存的是原名，渲染时才加前缀。
    assert!(options.contains(&"家里"), "{options:?}");
    assert!(!options.contains(&"Custom-家里"), "候选里不该是渲染后的名字：{options:?}");
}

/// 写操作必须带 CSRF。漏掉它是个安静的漏洞，所以钉一条。
#[tokio::test]
async fn 写操作要校验csrf() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (status, _, _) = api.post_without_csrf("/api/v1/me/custom/proxies", &alice).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

// ============ 页面与脚本的契约 ============

const PAGE: &str = include_str!("../../web/me-custom.html");
const SCRIPT: &str = include_str!("../../web/assets/api.js");

/// 页面能发出来，而且导航里有它。
///
/// 加了页面忘了登记 `PAGES`，症状是 404；登记了忘了加导航，
/// 症状是「功能做完了但没人找得到」——后者不会有人报错。
#[tokio::test]
async fn 页面发得出来且导航里有() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (status, body, _) = api.get_text("/me-custom.html", Some(&alice.cookie)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert!(body.contains("我的自定义节点"), "发出来的不是这一页：{body}");

    // 每一个个人中心页面都要能走到它。
    for page in ["me.html", "me-usage.html", "me-audit.html"] {
        let body = proxy_manager::web::page_body(page).expect("页面该在");
        assert!(body.contains("me-custom.html"), "{page} 的导航里没有自定义节点");
    }
}

/// **页面上的每个集合都要有脚本在填。**
///
/// 少一个的结果是那张表永远停在「加载中…」，而没有任何报错——
/// 本项目为这一类「服务端知道，界面不说」吃过亏。
#[tokio::test]
async fn 页面上的集合都有脚本在填() {
    let mut collections = Vec::new();
    let mut rest = PAGE;
    while let Some(at) = rest.find("data-pm-collection=\"") {
        rest = &rest[at + "data-pm-collection=\"".len()..];
        let name = &rest[..rest.find('"').expect("属性该闭合")];
        collections.push(name.to_string());
    }
    assert!(!collections.is_empty(), "页面上一个集合都没有，用例本身失效了");
    for name in collections {
        assert!(
            SCRIPT.contains(&format!("renderCollection(\"{name}\"")),
            "集合「{name}」没有对应的 renderCollection，那张表会永远停在「加载中…」"
        );
    }
}

/// 脚本里那个页面处理器**取的路径必须真的存在**。
#[tokio::test]
async fn 脚本取的端点存在() {
    assert!(SCRIPT.contains("\"me-custom.html\": {"), "api.js 里没有这一页的处理器");
    assert!(SCRIPT.contains("getJson(\"/me/custom\")"), "处理器没取 /me/custom");
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (status, _, _) = api.get("/api/v1/me/custom", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "脚本要取的端点不存在");
}

// ============ 分流规则 ============

const RULES_PAGE: &str = include_str!("../../web/me-custom-rules.html");

#[tokio::test]
async fn 规则整份替换并保序() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;

    let (status, body, _) = api
        .put_json(
            "/api/v1/me/custom/rules",
            &alice,
            json!({ "rules": [
                { "kind": "domain-suffix", "value": "First.Example" },
                { "kind": "ip-cidr", "value": "10.0.0.0/8" }
            ] }),
            Some(&alice.csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["count"], 2);

    let (_, body, _) = api.get("/api/v1/me/custom/rules", Some(&alice)).await;
    assert_eq!(body["enabled"], true);
    assert_eq!(body["group_name"], "Rules-alice");
    // 顺序就是优先级，读回来必须原样。
    assert_eq!(body["rules"][0]["value"], "first.example", "要规范化成小写：{body}");
    assert_eq!(body["rules"][1]["value"], "10.0.0.0/8");
}

/// 报错要说清**是第几条**。整份提交时只说「规则无效」，
/// 用户要在几十条里自己找是哪一条。
#[tokio::test]
async fn 规则报错要指出第几条() {
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let (status, body, _) = api
        .put_json(
            "/api/v1/me/custom/rules",
            &alice,
            json!({ "rules": [
                { "kind": "domain-suffix", "value": "ok.example" },
                { "kind": "ip-cidr", "value": "192.168.1.5/16" }
            ] }),
            Some(&alice.csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "{body}");
    let message = body["error"]["message"].as_str().unwrap_or_default();
    assert!(message.contains("第 2 条"), "要指出是第几条：{message}");
    // 而且要给出**正确的写法**，不能只说「错了」。
    assert!(message.contains("192.168.0.0/16"), "{message}");
}

/// 空表把那一行删掉，不留一个装着 `[]` 的行——
/// 「有没有规则」要看两处的话，两处迟早会给出不同的答案。
#[tokio::test]
async fn 清空规则等于删掉那一行() {
    use sqlx::Row;
    let api = api().await;
    let alice = api.user("alice", Role::User, "local").await;
    let csrf = alice.csrf.clone();
    let (status, _, _) = api
        .put_json(
            "/api/v1/me/custom/rules",
            &alice,
            json!({ "rules": [{ "kind": "domain", "value": "a.example" }] }),
            Some(&csrf),
            None,
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    let n: i64 = sqlx::query("SELECT count(*) FROM custom_rules")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(n, 1);

    let (status, _, _) = api
        .put_json("/api/v1/me/custom/rules", &alice, json!({ "rules": [] }), Some(&csrf), None)
        .await;
    assert_eq!(status, StatusCode::OK);
    let n: i64 = sqlx::query("SELECT count(*) FROM custom_rules")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(n, 0, "空表该把那一行删掉");
}

/// 页面与脚本的契约：集合有人填、端点存在、类型下拉与服务端的闭集对得上。
#[tokio::test]
async fn 规则页与脚本对得上() {
    assert!(SCRIPT.contains("\"me-custom-rules.html\": {"), "api.js 里没有这一页的处理器");
    assert!(SCRIPT.contains("getJson(\"/me/custom/rules\")"), "处理器没取 /me/custom/rules");
    assert!(
        SCRIPT.contains("renderCollection(\"custom-rules\""),
        "规则表没人填，会永远停在「加载中…」"
    );
    // **页面下拉里的每个类型，服务端都要认。** 少一个的症状是
    // 用户选了它、提交、收到一句「unknown variant」。
    for kind in ["domain-suffix", "domain", "domain-keyword", "ip-cidr", "ip-cidr6"] {
        assert!(RULES_PAGE.contains(&format!("value=\"{kind}\"")), "页面缺类型 {kind}");
        assert!(SCRIPT.contains(kind), "脚本的显示名表里缺 {kind}");
    }
}

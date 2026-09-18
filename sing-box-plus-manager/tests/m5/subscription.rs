//! 订阅（README §4.7、M6）。
//!
//! §4.7 点名了三个「缺一个都会在真实客户端上出问题」的响应细节：
//! `Subscription-Userinfo`、`Content-Disposition`、以及节点名。
//! 这一组逐条钉住它们，外加 token 的生命周期。

use axum::http::StatusCode;
use proxy_manager::sso::client::SsoIdentity;
use proxy_manager::sso::roster::Roster;

use super::harness::Api;

fn identity(account: &str) -> SsoIdentity {
    SsoIdentity {
        account: account.to_string(),
        display_name: account.to_string(),
        feishu_fs_id: None,
    }
}

async fn login(api: &Api, account: &str) -> proxy_manager::sso::LoggedIn {
    proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity(account),
        time::Duration::hours(8),
    )
    .await
    .unwrap()
}

// ============ token 的生命周期 ============

/// 首次登录就拿到订阅 token，**明文只在这一次出现**。
#[tokio::test]
async fn 首次登录就发订阅token() {
    let api = Api::with_subscription(&[], &["slot-01", "slot-02"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;

    let first = login(&api, "alice").await;
    let token = first.subscription.expect("登录应当顺带发一条订阅 token");
    assert!(token.newly_created);
    assert_eq!(token.plaintext.len(), 64, "32 字节十六进制");

    // 第二次登录**不再发新的**，但**算得出同一个**——token 是派生的，
    // 所以本人随时能再看一次自己的订阅地址，而明文一个字节都没落过库。
    let second = login(&api, "alice").await;
    let again = second.subscription.unwrap();
    assert!(!again.newly_created, "不该发第二条");
    assert_eq!(again.plaintext, token.plaintext, "应当算出同一个 token");
}

/// **一个用户同一时刻只能有一条未吊销的 token。**
///
/// 没有这条，一次重复建号会悄悄发出第二条而第一条仍然有效——
/// 于是「吊销一条」不等于断掉这个人。
#[tokio::test]
async fn 一个人只有一条有效token() {
    use sqlx::Row;
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    login(&api, "alice").await;
    login(&api, "alice").await;

    let n: i64 = sqlx::query(
        "SELECT count(*) FROM subscription_tokens WHERE user_id = ? AND revoked_at IS NULL",
    )
    .bind(logged_in.user_id)
    .fetch_one(api.store.readers())
    .await
    .unwrap()
    .get(0);
    assert_eq!(n, 1);
}

// ============ 取订阅 ============

#[tokio::test]
async fn 用token取得到自己的节点() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;

    let (status, body, headers) = api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await;
    assert_eq!(status, StatusCode::OK);

    // 正文是 Clash / mihomo YAML——地址以 .yaml 结尾，内容就得真是 YAML。
    assert!(body.contains("proxies:"), "实际：{body}");
    assert!(body.contains("proxy-groups:"), "实际：{body}");
    assert!(body.contains("rules:"), "实际：{body}");
    assert_eq!(body.matches("type: ss").count(), 2, "两条入口两个节点：{body}");
    assert!(body.contains("2022-blake3-aes-128-gcm"));
    // 组与规则来自**旧站模板**，逐字节搬过来的。
    assert!(body.contains(r#"name: "手动选择""#), "代理组应当来自旧模板：{body}");
    assert!(body.contains("海外AI"), "旧站那 290 行规则要在：{body}");
    assert!(body.contains("tun:") && body.contains("dns:"), "head 也要搬过来：{body}");
    // **密码必须加引号**：base64 含 + / =，而 = 开头在 YAML 里有特殊含义。
    assert!(body.contains(r#"password: ""#), "密码没加引号：{body}");

    assert_eq!(headers.get("content-type").unwrap(), "text/yaml; charset=utf-8");
    // 订阅是凭据，不进任何缓存。
    assert_eq!(headers.get("cache-control").unwrap(), "private, no-store");
    assert_eq!(headers.get("referrer-policy").unwrap(), "no-referrer");
}

/// 路径形状写死：`Proxy-<64位十六进制>.yaml`，别的一律 404。
///
/// 宽松匹配意味着任何 `/sub/*` 都会打到数据库上。
#[tokio::test]
async fn 订阅路径形状写死() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let token = login(&api, "alice").await.subscription.unwrap().plaintext;

    for bad in [
        format!("/sub/{token}"),                     // 缺前缀与扩展名
        format!("/sub/Proxy-{token}"),               // 缺扩展名
        format!("/sub/{token}.yaml"),                // 缺前缀
        format!("/sub/Proxy-{}.yaml", &token[..63]), // 短一位
        "/sub/Proxy-....yaml".to_string(),
    ] {
        assert_eq!(api.get_text(&bad, None).await.0, StatusCode::NOT_FOUND, "{bad} 本不该被接受");
    }
    assert_eq!(api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await.0, StatusCode::OK);
}

/// **`Subscription-Userinfo`**：Clash / mihomo 靠它显示流量与到期。
///
/// §4.7 有一条专门的警告：别照抄不做配额的系统，那种系统里 `total` 是给客户端看的
/// 假值；本项目有真实闸断，填假值会让客户端显示与实际行为对不上。
#[tokio::test]
async fn 带着真实的用量与额度头() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;

    let (_, _, headers) = api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await;
    let info = headers.get("subscription-userinfo").unwrap().to_str().unwrap();
    for key in ["upload=", "download=", "total=", "expire="] {
        assert!(info.contains(key), "缺 {key}：{info}");
    }
    // total 是**真实**额度：normal 档十进制 1 TB（D23）。
    assert!(info.contains("total=1000000000000"), "total 必须是真实额度：{info}");
    // expire 是周期结束时刻，不能是 0——0 会让客户端显示「已过期」。
    let expire: i64 = info.split("expire=").nth(1).unwrap().trim().parse().unwrap();
    assert!(expire > 1_700_000_000, "expire 应当是将来的 unix 秒：{expire}");
}

/// **`Content-Disposition` 里绝不能有 token。**
///
/// 生产教训写在 §4.7：最初拿 opaque token 当文件名，客户端导入后显示成一串乱码；
/// 等发现要改名时，已导入的旧订阅还得让每个人手动删除重导。
#[tokio::test]
async fn 文件名来自账号名而不是token() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;

    let (_, _, headers) = api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await;
    let disposition = headers.get("content-disposition").unwrap().to_str().unwrap();
    assert!(disposition.contains("Proxy-alice.yaml"), "实际：{disposition}");
    assert!(disposition.contains("filename*=UTF-8''"), "老客户端要 ASCII，新的要 RFC 5987");
    assert!(!disposition.contains(&token), "**响应头绝不含 token**：{disposition}");
}

/// 吊销之后立刻失效，且**不区分**失败原因。
///
/// 区分开来就等于给拿着一个无效 token 的人一个探测接口。
#[tokio::test]
async fn 吊销之后立刻失效() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;
    assert_eq!(api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await.0, StatusCode::OK);

    let n =
        proxy_manager::subscription::revoke_for_user(&api.store, logged_in.user_id).await.unwrap();
    assert_eq!(n, 1);
    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND,
        "吊销后立刻失效"
    );

    // 重发拿到的是**不同的** token：generation 加一，旧的算不出来也查不到。
    let reissued =
        proxy_manager::subscription::issue_for_user(&api.store, logged_in.user_id).await.unwrap();
    assert!(reissued.newly_created);
    assert_ne!(reissued.plaintext, token, "重发必须换一个，否则吊销等于没吊");
    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{}.yaml", reissued.plaintext), None).await.0,
        StatusCode::OK
    );
    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND,
        "旧的仍然失效"
    );

    // 不存在的 token 与被吊销的 token **返回同一个东西**。
    let bogus = "f".repeat(64);
    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{bogus}.yaml"), None).await.0,
        StatusCode::NOT_FOUND
    );
}

/// 停用的账号取不到订阅——与会话那边同一条。
#[tokio::test]
async fn 停用的账号取不到订阅() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;

    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET status = 'disabled' WHERE user_id = ?")
        .bind(logged_in.user_id)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND
    );
}

/// 没配 `[subscription]` 时 `/sub/…` 根本不挂载。
#[tokio::test]
async fn 没配订阅时端点不存在() {
    let api = Api::new().await;
    let bogus = "a".repeat(64);
    assert_eq!(
        api.get_text(&format!("/sub/Proxy-{bogus}.yaml"), None).await.0,
        StatusCode::NOT_FOUND
    );
}

/// **凭据文件里没有这个身份时返回空订阅，不是编一个密码。**
///
/// 编一个出来的后果是用户导入之后连不上，而客户端侧的错误只会是「握手失败」——
/// 查不到这里。空订阅至少让人看得出「我没有节点」。
#[tokio::test]
async fn 凭据缺失时是空订阅而不是假密码() {
    // 池子里有 slot-99，但凭据文件里只写了 slot-01。
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-99"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.unwrap().plaintext;

    let (status, body, _) = api.get_text(&format!("/sub/Proxy-{token}.yaml"), None).await;
    assert_eq!(status, StatusCode::OK);
    // **明说的空配置**，不是编一个密码，也不是一份空文件。
    assert!(body.contains("proxies: []"), "实际：{body}");
    assert!(body.contains("还没有分配到节点"), "要说清为什么是空的：{body}");
    assert!(!body.contains("type: ss"), "不该编出任何节点");
}

// ============ 渲染的纯函数部分 ============

/// uPSK 是 base64 文本，含 `+` `/` `=`。**必须 URL 编码**——
/// 不编码的话 `/` 会被客户端当成路径分隔符，密码从中间被截断。
#[test]
fn 密码在yaml里必须加引号() {
    let entries = vec![proxy_manager::config::Entry {
        name: "HK".into(),
        host: "203.0.113.1".into(),
        port: 65002,
        node_id: "n".into(),
    }];
    let credentials = proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "a+b/c=".into(),
        upsk: [("slot-01".to_string(), "x+y/z=".to_string())].into_iter().collect(),
    };
    let groups = vec![proxy_manager::config::ProxyGroup {
        name: "手动选择".into(),
        icon: None,
        proxies: vec!["HK".into(), "DIRECT".into()],
    }];
    let yaml = proxy_manager::subscription::render::clash_yaml(
        &entries,
        &groups,
        &credentials,
        "slot-01",
        "Proxy-alice",
    );
    // SS2022 带 EIH 时 Clash 的 password 是 `<iPSK>:<uPSK>`。
    assert!(yaml.contains(r#"password: "a+b/c=:x+y/z=""#), "实际：{yaml}");
    // **不加引号会整份解析失败**：base64 含 `=`，而 `=` 开头在 YAML 里有特殊含义，
    // 而客户端只会说「订阅格式错误」，指不到这里。
    assert!(!yaml.contains("password: a+b"), "密码不能裸写：{yaml}");
}

/// 代理组名不得与任何节点名相同——同一个命名空间，撞了就成了指向自己的组，
/// 表现是「选了没反应」。配置校验就该挡住它。
#[test]
fn 组名与节点同名会被配置校验挡住() {
    let toml = r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[entries]]
name = "HK"
host = "203.0.113.1"
port = 65002
node_id = "n"
[[groups]]
name = "HK"
proxies = ["HK"]
"#;
    let config: proxy_manager::config::SubscriptionConfig = toml::from_str(toml).unwrap();
    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("与某个入口同名"), "实际：{error}");
}

/// 组成员写错一个名字，Clash 会整份解析失败而只说「订阅格式错误」。
/// 配置校验必须在那之前挡住。
#[test]
fn 组里引用不存在的节点会被挡住() {
    let toml = r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[entries]]
name = "HK"
host = "203.0.113.1"
port = 65002
node_id = "n"
[[groups]]
name = "手动选择"
proxies = ["HK", "JP"]
"#;
    let config: proxy_manager::config::SubscriptionConfig = toml::from_str(toml).unwrap();
    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("既不是入口也不是内置目标"), "实际：{error}");
}

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

    // 拿内网那份：它走 SS，下面几条断言里有 cipher 与 password。
    // 公网那份走 VLESS，由「两种拨法」那条用例逐字段核。
    let (status, body, headers) = api.get_text(&format!("/sub/proxyLan-{token}.yaml"), None).await;
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
        format!("/sub/{token}"),                        // 缺前缀与扩展名
        format!("/sub/proxyWan-{token}"),               // 缺扩展名
        format!("/sub/{token}.yaml"),                   // 缺前缀
        format!("/sub/proxyWan-{}.yaml", &token[..63]), // 短一位
        "/sub/proxyWan-....yaml".to_string(),
    ] {
        assert_eq!(api.get_text(&bad, None).await.0, StatusCode::NOT_FOUND, "{bad} 本不该被接受");
    }
    assert_eq!(api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await.0, StatusCode::OK);
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

    let (_, _, headers) = api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await;
    let info = headers.get("subscription-userinfo").unwrap().to_str().unwrap();
    for key in ["upload=", "download=", "total=", "expire="] {
        assert!(info.contains(key), "缺 {key}：{info}");
    }
    // total 是**真实**额度：normal 档十进制 1 TB（D23）。
    assert!(info.contains("total=1099511627776"), "total 必须是真实额度：{info}");
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

    let (_, _, headers) = api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await;
    let disposition = headers.get("content-disposition").unwrap().to_str().unwrap();
    assert!(disposition.contains("proxyWan-alice.yaml"), "实际：{disposition}");
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
    assert_eq!(api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await.0, StatusCode::OK);

    let n =
        proxy_manager::subscription::revoke_for_user(&api.store, logged_in.user_id).await.unwrap();
    assert_eq!(n, 1);
    assert_eq!(
        api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND,
        "吊销后立刻失效"
    );

    // 重发拿到的是**不同的** token：generation 加一，旧的算不出来也查不到。
    let reissued =
        proxy_manager::subscription::issue_for_user(&api.store, logged_in.user_id).await.unwrap();
    assert!(reissued.newly_created);
    assert_ne!(reissued.plaintext, token, "重发必须换一个，否则吊销等于没吊");
    assert_eq!(
        api.get_text(&format!("/sub/proxyWan-{}.yaml", reissued.plaintext), None).await.0,
        StatusCode::OK
    );
    assert_eq!(
        api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND,
        "旧的仍然失效"
    );

    // 不存在的 token 与被吊销的 token **返回同一个东西**。
    let bogus = "f".repeat(64);
    assert_eq!(
        api.get_text(&format!("/sub/proxyWan-{bogus}.yaml"), None).await.0,
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
        api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await.0,
        StatusCode::NOT_FOUND
    );
}

/// 没配 `[subscription]` 时 `/sub/…` 根本不挂载。
#[tokio::test]
async fn 没配订阅时端点不存在() {
    let api = Api::new().await;
    let bogus = "a".repeat(64);
    assert_eq!(
        api.get_text(&format!("/sub/proxyWan-{bogus}.yaml"), None).await.0,
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

    let (status, body, _) = api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await;
    assert_eq!(status, StatusCode::OK);
    // **明说的空配置**，不是编一个密码，也不是一份空文件。
    assert!(body.contains("proxies: []"), "实际：{body}");
    assert!(body.contains("还没有分配到节点"), "要说清为什么是空的：{body}");
    assert!(!body.contains("type: ss"), "不该编出任何节点");
}

// ============ 控制台上的订阅（`/api/v1/me/subscription`） ============

fn actor(logged_in: &proxy_manager::sso::LoggedIn) -> super::harness::Actor {
    super::harness::Actor {
        user_id: logged_in.user_id,
        session_pk: logged_in.session.session_pk,
        cookie: logged_in.session.cookie_value.clone(),
        csrf: logged_in.session.csrf_token.clone(),
    }
}

/// 本人在控制台上看得到自己的订阅地址。
///
/// 这一条是 M6 对使用者那一半：token 派生得出来，所以**随时能再看一次**，
/// 不需要管理员上机器跑一条命令再把地址贴给他——那条路径里地址会经过
/// 聊天记录、工单和剪贴板，而它本身就是凭据。
#[tokio::test]
async fn 本人看得到自己的订阅地址() {
    let api = Api::with_subscription(&[], &["slot-01", "slot-02"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.as_ref().unwrap().plaintext.clone();

    let (status, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&logged_in))).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["enabled"], true);
    let subs = body["subscriptions"].as_array().expect("要有 subscriptions");
    assert_eq!(subs.len(), 2, "两种拨法各一条：{body}");
    assert_eq!(subs[0]["url"], format!("https://pm.example.com/sub/proxyWan-{token}.yaml"));
    assert_eq!(subs[0]["profile_name"], "proxyWan-alice");
    assert_eq!(subs[1]["url"], format!("https://pm.example.com/sub/proxyLan-{token}.yaml"));
    assert_eq!(subs[1]["profile_name"], "proxyLan-alice");
    // **两条用的是同一个 token**：它们是同一份订阅的两种拨法，不是两份订阅。
    assert!(subs[0]["url"].as_str().unwrap().contains(&token));
    assert!(subs[1]["url"].as_str().unwrap().contains(&token));
    assert_eq!(body["identity"], "slot-01");
    assert_eq!(body["entry_count"], 2);
}

/// **个人端点不带连接细节。**
///
/// 地址与端口写在订阅正文里，那是给客户端的。页面上再列一遍等于把同一份
/// 凭据材料多放一个地方，而页面会被截图、会被投屏。§4.11 的同一条。
#[tokio::test]
async fn 个人端点只给入口名不给地址端口() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;

    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&logged_in))).await;
    let entries = body["entries"].as_array().expect("要有 entries");
    assert_eq!(entries.len(), 2);
    for entry in entries {
        assert!(entry["name"].is_string(), "要有显示名：{entry}");
        assert!(entry["node_id"].is_string(), "要有落点：{entry}");
        assert!(entry.get("host").is_none(), "不该带地址：{entry}");
        assert!(entry.get("port").is_none(), "不该带端口：{entry}");
    }
    // 整份响应里也不该出现那两个地址。
    let text = body.to_string();
    assert!(!text.contains("203.0.113.1"), "响应里漏了入口地址：{text}");
    assert!(!text.contains("198.51.100.1"), "响应里漏了入口地址：{text}");
    assert!(!text.contains("65002"), "响应里漏了入口端口：{text}");
}

/// **`/me` 里不带 token。**
///
/// 前端每一页都会取一次 `/me` 来判定 live 模式，塞进去等于把订阅地址复制进
/// 每一个页面的响应与内存里。这条是那个决定的回归测试——
/// 把它并进 `/me` 会很方便，而方便正是它会被并进去的原因。
#[tokio::test]
async fn me里不带订阅token() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.as_ref().unwrap().plaintext.clone();

    let (status, body, _) = api.get("/api/v1/me", Some(&actor(&logged_in))).await;
    assert_eq!(status, StatusCode::OK);
    assert!(!body.to_string().contains(&token), "/me 不该带订阅 token");
}

/// **查看不是签发。**
///
/// GET 写库的话，任何人刷一下页面就把自己的地址换掉了，而「谁在什么时候
/// 拿到了订阅」这条线索也就没了。吊销之后回 `url: null`，不偷偷补一条新的。
#[tokio::test]
async fn 查看订阅不签发新token() {
    use sqlx::Row;
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let actor = actor(&logged_in);

    let (_, first, _) = api.get("/api/v1/me/subscription", Some(&actor)).await;
    let (_, again, _) = api.get("/api/v1/me/subscription", Some(&actor)).await;
    assert_eq!(first["subscriptions"], again["subscriptions"], "两次查看应当是同一批地址");

    proxy_manager::subscription::revoke_for_user(&api.store, logged_in.user_id).await.unwrap();
    let (_, after, _) = api.get("/api/v1/me/subscription", Some(&actor)).await;
    for sub in after["subscriptions"].as_array().unwrap() {
        assert!(sub["url"].is_null(), "吊销之后不该还有地址：{after}");
    }
    assert_eq!(after["enabled"], true, "能力仍然是启用的，只是这个人没有 token");

    let rows: i64 = sqlx::query("SELECT count(*) FROM subscription_tokens WHERE user_id = ?")
        .bind(logged_in.user_id)
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(rows, 1, "三次 GET 之后仍然只有登录时发的那一条");
}

/// 别人的订阅只有管理员看得到。
#[tokio::test]
async fn 别人的订阅要管理员() {
    let api = Api::with_subscription(&["boss"], &["slot-01", "slot-02"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;
    let alice = login(&api, "alice").await;
    let boss = login(&api, "boss").await;

    let (status, body, _) =
        api.get(&format!("/api/v1/subscriptions/{}", boss.user_id), Some(&actor(&alice))).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "{body}");

    // 管理员看得到，而且与本人自己看到的是同一个地址。
    let (status, seen_by_admin, _) =
        api.get(&format!("/api/v1/subscriptions/{}", alice.user_id), Some(&actor(&boss))).await;
    assert_eq!(status, StatusCode::OK);
    let (_, seen_by_self, _) = api.get("/api/v1/me/subscription", Some(&actor(&alice))).await;
    assert_eq!(seen_by_admin["subscriptions"], seen_by_self["subscriptions"]);
    assert_eq!(seen_by_admin["subscriptions"][0]["profile_name"], "proxyWan-alice");

    let (status, _, _) = api.get("/api/v1/subscriptions/999999", Some(&actor(&boss))).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "没有这个用户");
}

/// 没配 `[subscription]` 时回 `enabled: false`，**不是错误也不是 404**。
///
/// 这是一个确定的事实，页面据此说「服务端没配订阅出口」。
/// 回错误会被读成「加载失败，刷新试试」，而刷多少次都一样。
#[tokio::test]
async fn 没配订阅时明确回未启用() {
    let api = Api::new().await;
    let user = api.user("alice", proxy_manager::api::session::Role::User, "local").await;
    let (status, body, _) = api.get("/api/v1/me/subscription", Some(&user)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["enabled"], false);
    assert!(body.get("url").is_none(), "没配的时候不该有 url 这个字段：{body}");
}

// ============ 页面与端点的机械核对 ============

/// **服务端知道这份订阅能不能用，就必须说出来。**
///
/// 三种不可用状态的处置完全不同——缺身份是容量问题，缺 uPSK 是部署问题，
/// 缺 token 是签发问题——所以它们不能折成同一句「暂时不可用」。
#[tokio::test]
async fn 不可用的理由分得开() {
    // 1) 一切正常。
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let alice = login(&api, "alice").await;
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&alice))).await;
    assert_eq!(body["usable"], true, "{body}");
    assert!(body["unusable_reason"].is_null(), "{body}");
    // **能用就一定有地址。** 页面上的「有效」徽标只看 `usable`，
    // 这条不变式就是它可以不再另外判一次 `url` 的理由。
    for sub in body["subscriptions"].as_array().unwrap() {
        assert!(sub["url"].is_string(), "usable 为真却有一种拨法没有地址：{body}");
    }

    // 2) 吊销之后：有身份、有凭据，但没有地址。
    proxy_manager::subscription::revoke_for_user(&api.store, alice.user_id).await.unwrap();
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&alice))).await;
    assert_eq!(body["usable"], false);
    assert_eq!(body["unusable_reason"], "no_token");

    // 3) 池子里是 slot-99，凭据文件里只有 slot-01：**服务端已经知道订阅会是空的**。
    //    这一条是本轮对抗审查揪出来的——原先页面照样报「N 条入口可用」。
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-99"]).await;
    let bob = login(&api, "bob").await;
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&bob))).await;
    assert_eq!(body["identity"], "slot-99", "这个人确实领到了身份");
    assert_eq!(body["usable"], false, "但凭据文件里没有它：{body}");
    assert_eq!(body["unusable_reason"], "no_credential");

    // 4) 池子是空的：登录成功但没领到身份。
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    let carol = login(&api, "carol").await;
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&carol))).await;
    assert!(body["identity"].is_null(), "{body}");
    assert_eq!(body["unusable_reason"], "no_identity");
}

/// **服务端能给出的每一种理由，页面上都得有一条对应的说明。**
///
/// 少一条的后果不是报错，是**一片空白**：没有地址、没有解释、没有下一步。
/// 那个人只会看到一个「—」，然后刷新几次，然后来问「是不是坏了」。
///
/// 这条核的是集合的封闭性，所以新增一个理由而忘了写文案时它会先红。
#[test]
fn 每种不可用理由都配了文案() {
    let html = proxy_manager::web::page_body("me.html").expect("me.html 必须嵌在二进制里");
    let declared: std::collections::BTreeSet<String> = attr_values(html, "data-pm-unusable")
        .iter()
        .flat_map(|value| value.split_whitespace().map(str::to_string).collect::<Vec<_>>())
        .collect();
    assert!(!declared.is_empty(), "一条 data-pm-unusable 都没扫到：扫描器多半坏了");

    for reason in proxy_manager::api::routes::UNUSABLE_REASONS {
        assert!(
            declared.contains(*reason),
            "me.html 里没有 {reason:?} 的说明：那个人会看到一片空白而不是一句话"
        );
    }
    // 反过来也要核：页面上写了一条服务端**永远不会给**的理由，
    // 那条文案就是死的——而它看起来和活的一模一样。
    for reason in &declared {
        assert!(
            proxy_manager::api::routes::UNUSABLE_REASONS.contains(&reason.as_str()),
            "me.html 写了 {reason:?} 的说明，但服务端永远不会给出这个理由"
        );
    }
}

/// 节点卡上**不许有状态徽标**。
///
/// 这张卡上只有用量，没有任何可用性字段。写一个「可用」上去就是替服务端编答案：
/// 节点掉线、采集失败、额度闸断时它照样是绿的。
/// 这条是那次改动的回归测试——加回去很容易，因为那样"好看"。
///
/// 2026-09-20 这张卡从「列入口」改成「列节点 + 公网/内网用量」，
/// 判据跟着从 `name` 换成 `node_id`，但要守的东西一个字没变。
#[test]
fn 节点卡上没有编出来的状态徽标() {
    let html = proxy_manager::web::page_body("me.html").unwrap();
    let (_, inside) = split_templates(html);
    // **只看节点卡那一个模板。** 页面上现在有两个模板，另一个（订阅地址）里
    // 那枚徽标绑的是 `profile_name`，有数据支撑，不该被这条误伤。
    let card = inside
        .split("pm-node-card")
        .nth(1)
        .expect("节点卡模板没扫到：扫描器坏了，或者页面结构变了");
    assert!(card.contains("data-pm=\"node_id\""), "扫到的不是节点卡模板：{card}");
    assert!(
        !card.contains("wsk-badge"),
        "节点卡模板里出现了徽标——服务端没有任何字段能支撑它：{card}"
    );
}

/// 把 `<template>` 里外分开。
///
/// 模板里的绑定是**按集合成员**求值的（`renderRows` 克隆之后对每一行 applyBindings），
/// 模板外的是按整份响应求值的。两者取值的对象不同，所以必须分开核。
fn split_templates(html: &str) -> (String, String) {
    let (mut outside, mut inside) = (String::new(), String::new());
    let mut rest = html;
    loop {
        let Some(open) = rest.find("<template") else {
            outside.push_str(rest);
            return (outside, inside);
        };
        outside.push_str(&rest[..open]);
        let after = &rest[open..];
        let Some(close) = after.find("</template>") else {
            inside.push_str(after);
            return (outside, inside);
        };
        inside.push_str(&after[..close]);
        rest = &after[close + "</template>".len()..];
    }
}

fn attr_values(html: &str, attr: &str) -> Vec<String> {
    let needle = format!("{attr}=\"");
    let mut out = Vec::new();
    let mut rest = html;
    while let Some(index) = rest.find(&needle) {
        let after = &rest[index + needle.len()..];
        let end = after.find('"').expect("属性没闭合");
        out.push(after[..end].to_string());
        rest = &after[end..];
    }
    out
}

fn binding_paths(html: &str) -> Vec<String> {
    let mut paths = Vec::new();
    for attr in ["data-pm", "data-pm-bytes", "data-pm-exact", "data-pm-digits"] {
        paths.extend(attr_values(html, attr));
    }
    paths.sort();
    paths.dedup();
    paths
}

fn pick<'a>(value: &'a serde_json::Value, path: &str) -> Option<&'a serde_json::Value> {
    let mut current = value;
    for key in path.split('.') {
        current = current.get(key)?;
    }
    Some(current)
}

/// **me.html 上的每一个绑定，都要在端点真的返回的 JSON 里取得到值。**
///
/// 这一页的绑定横跨三个端点（`/me` 给身份与额度，`/me/subscription` 给地址，
/// `/me/usage` 给按节点的公网/内网用量），
/// 而 `api.js` 把它们合成一个对象再铺上去。合并口径是**手写的**，
/// 于是这里有三样东西必须同时对得上：页面上的路径、合并后的对象、两个端点的字段名。
///
/// 任何一样改了而另外两样没跟上，症状都是同一个：**那一格显示成「—」**。
/// 它不报错、不进日志，看起来就像「这个人没有数据」。这条用例就是为了让它先红。
#[tokio::test]
async fn me页面上的每个绑定都取得到值() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let actor = actor(&logged_in);

    let (_, me, _) = api.get("/api/v1/me", Some(&actor)).await;
    let (_, sub, _) = api.get("/api/v1/me/subscription", Some(&actor)).await;
    let (_, usage, _) = api.get("/api/v1/me/usage", Some(&actor)).await;

    // **与 `web/assets/api.js` 的 `PAGES["me.html"].render` 是同一个合并口径。**
    // 那边改了这边不改，这条用例就是那次改动的报警器。
    let mut merged = me.clone();
    merged["identity"] = sub["identity"].clone();
    merged["entry_count"] = sub["entry_count"].clone();

    let html = proxy_manager::web::page_body("me.html").expect("me.html 必须嵌在二进制里");
    let (outside, inside) = split_templates(html);

    // **先证明扫出来的东西不是空的。** 扫描器悄悄返回空列表的话，
    // 下面那个循环一次都不执行，这条用例就变成一句恒真的断言——
    // 而它看起来仍然是绿的。
    let outside_paths = binding_paths(&outside);
    // **这一条是扫描器的自检，不是页面的规格。** 它挡的是「扫描器悄悄返回空列表，
    // 于是下面那个循环一次都不执行」那种恒真的绿。
    //
    // 判据从「至少 N 条」换成「额度那四格都在」（2026-09-20）：数字那版每次
    // 修剪页面文案都要跟着调，调着调着就没人记得它原本在防什么了。
    // 额度区是这一页的固定家具，拿它当自检比拿一个魔数稳。
    let cycle: Vec<&String> =
        outside_paths.iter().filter(|path| path.starts_with("cycle.")).collect();
    assert_eq!(cycle.len(), 4, "额度那四格没扫全，扫描器多半坏了：{outside_paths:?}");
    // 订阅地址现在在模板里（一种拨法一行），所以它在 inside_paths 里。
    assert!(outside_paths.contains(&"cycle.remaining_bytes".to_string()), "额度那一格没扫到");

    for path in outside_paths {
        let value = pick(&merged, &path)
            .unwrap_or_else(|| panic!("me.html 绑了 {path:?}，但合并后的响应里没有这个字段"));
        assert!(!value.is_null(), "me.html 绑的 {path:?} 取到了 null：页面上会显示成「—」");
    }

    // 模板里的绑定按集合成员求值。
    let inside_paths = binding_paths(&inside);
    assert_eq!(
        inside_paths,
        vec![
            "node_id".to_string(),
            "note".to_string(),
            "profile_name".to_string(),
            "sources.0.label".to_string(),
            "sources.0.total_bytes".to_string(),
            "sources.1.label".to_string(),
            "sources.1.total_bytes".to_string(),
            "url".to_string()
        ],
        "模板里的绑定变了：要么页面改了，要么扫描器把模板内外分错了"
    );
    // 订阅那三个绑定按 subscriptions 的元素求值。
    let dialing = &sub["subscriptions"][0];
    assert!(dialing.is_object(), "至少要有一种拨法：{sub}");
    for path in ["note", "profile_name", "url"] {
        let value = pick(dialing, path)
            .unwrap_or_else(|| panic!("me.html 的模板绑了 {path:?}，但拨法元素里没有"));
        assert!(!value.is_null(), "拨法绑的 {path:?} 取到了 null");
    }
    // **节点卡按下标绑两条来源**（`sources.0` / `sources.1`），所以
    // 「每个节点恒有一条来源对应一种拨法」必须由服务端保证，
    // 不能靠这个人恰好两种都用过。少一条的话页面上那一格是「—」，
    // 不报错、不进日志，看起来就像「这个人没用过公网」。
    //
    // 这里验的是**恒定那一半**：没有任何流量时，按来源那张表照样一种拨法一条。
    // 节点元素里的同一份列表由同一个函数产出（`split_by_source`），
    // 它带着真实流量的样子由 M4 的 `个人用量按来源分且标签取自配置` 钉住——
    // 那一组有 `ledger_harness`，能让账本行由真实结算产生，而这个二进制没有。
    let sources = usage["sources"].as_array().expect("/me/usage 必须有 sources");
    assert_eq!(
        sources.len(),
        2,
        "夹具配了两种拨法，按来源那张表就该恒有两条（与有没有流量无关）：{usage}"
    );
    for source in sources {
        for key in ["label", "total_bytes"] {
            assert!(!source[key].is_null(), "来源缺 {key}：{source}");
        }
    }
}

/// 集合名两边必须是同一个串。
///
/// `renderCollection` 找不到容器时**什么都不做，也不报错**——
/// 页面上留着的是 HTML 里那两张示例卡片，看起来完全正常，
/// 而它们是编的。这是本项目最不能接受的那一类失败。
#[test]
fn me页面的集合名与脚本里的一致() {
    let html = proxy_manager::web::page_body("me.html").unwrap();
    let (script, _) = proxy_manager::web::asset("assets/api.js").unwrap();
    let names = attr_values(html, "data-pm-collection");
    assert_eq!(
        names,
        vec!["subscriptions".to_string(), "me-nodes-usage".to_string()],
        "me.html 的集合名变了"
    );
    for name in names {
        assert!(
            script.contains(&format!("renderCollection(\"{name}\"")),
            "api.js 里没有 renderCollection(\"{name}\")：容器找不到时它静默不动，\
             页面上会留着 HTML 里那两张示例卡片"
        );
    }
}

// ============ 渲染的纯函数部分 ============

/// 一份与生产同形的最小配置：一批入口 + 两种到达方式。
///
/// **两种拨法的协议不同**，与生产一致：内网走 SS，公网走 VLESS（SS 容易被墙）。
/// 入口名两边逐字相同——代理组的成员写的就是这些名字。
fn fixture_config() -> proxy_manager::config::SubscriptionConfig {
    toml::from_str(
        r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[sources]]
prefix = "proxyLan-"
host = "198.51.100.1"
note = "内网"
transport = "ss"
[[sources]]
prefix = "proxyWan-"
host = "203.0.113.1"
note = "公网"
transport = "vless"
[[entries]]
name = "HK"
node_id = "n"
ports = { ss = 65002, vless = 65081 }
[[groups]]
name = "手动选择"
proxies = ["HK", "DIRECT"]
[vless]
servername = "www.example.com"
client_fingerprint = "chrome"
flow = "xtls-rprx-vision"
packet_encoding = "xudp"
[vless.reality.n]
public_key = "cHVibGljLWtleS1mb3ItdGVzdHMtMDAwMDAwMDAwMDA"
short_id = "01234567"
"#,
    )
    .expect("夹具配置必须解析得动")
}

/// 走 SS 的那种拨法。渲染 SS 的用例一律从这里取，不要写 `sources[0]`——
/// 顺序一变就会静默换成另一种协议，而断言只会报「密码不在里面」。
fn ss_source(
    config: &proxy_manager::config::SubscriptionConfig,
) -> &proxy_manager::config::EntrySource {
    config
        .sources
        .iter()
        .find(|s| s.transport == proxy_manager::config::Transport::Ss)
        .expect("夹具里应当有一种 SS 拨法")
}

/// 走 VLESS 的那种拨法。
fn vless_source(
    config: &proxy_manager::config::SubscriptionConfig,
) -> &proxy_manager::config::EntrySource {
    config
        .sources
        .iter()
        .find(|s| s.transport == proxy_manager::config::Transport::Vless)
        .expect("夹具里应当有一种 VLESS 拨法")
}

/// uPSK 是 base64 文本，含 `+` `/` `=`。**必须 URL 编码**——
/// 不编码的话 `/` 会被客户端当成路径分隔符，密码从中间被截断。
#[test]
fn 密码在yaml里必须加引号() {
    let config = fixture_config();
    let credentials = proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "a+b/c=".into(),
        upsk: [("slot-01".to_string(), "x+y/z=".to_string())].into_iter().collect(),
        uuid: [("slot-01".to_string(), "9ba335c9-0d3e-48a4-9f2e-28614c264a2d".to_string())]
            .into_iter()
            .collect(),
    };
    // 这个夹具里的节点没有级别要求，所以全可见。级别过滤另有用例。
    let visible = std::collections::BTreeSet::from(["n"]);
    let yaml = proxy_manager::subscription::render::clash_yaml(
        &config,
        ss_source(&config),
        &credentials,
        "slot-01",
        "proxyWan-alice",
        &visible,
        proxy_manager::subscription::custom::CustomNodes::NONE,
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
[[sources]]
prefix = "proxyWan-"
host = "203.0.113.1"
transport = "ss"
[[entries]]
name = "HK"
node_id = "n"
ports = { ss = 65002 }
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
[[sources]]
prefix = "proxyWan-"
host = "203.0.113.1"
transport = "ss"
[[entries]]
name = "HK"
node_id = "n"
ports = { ss = 65002 }
[[groups]]
name = "手动选择"
proxies = ["HK", "JP"]
"#;
    let config: proxy_manager::config::SubscriptionConfig = toml::from_str(toml).unwrap();
    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("既不是入口也不是内置目标"), "实际：{error}");
}

// ============ 两种拨法 ============

/// **同一个 token，两种拨法；差的是协议与 host，别的一律相同。**
///
/// 2026-09-20 之前这条用例叫「只差一个 host」，那时两种拨法确实只差客户端拨哪个
/// 地址。现在内网继续走 SS、公网改走 VLESS（SS 容易被墙），于是「只差 host」
/// 那半句不再成立——但**另外半句更要紧了**：入口名、代理组、规则、token
/// 仍然是同一套，一份都不许抄两遍。抄两遍的下场是它们在某次改端口时分家，
/// 而两份订阅里的节点名一模一样，看不出来。
#[tokio::test]
async fn 两种拨法差协议但共用入口名与规则() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.as_ref().unwrap().plaintext.clone();

    let (wan_status, wan, _) = api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await;
    let (lan_status, lan, _) = api.get_text(&format!("/sub/proxyLan-{token}.yaml"), None).await;
    assert_eq!(wan_status, StatusCode::OK);
    assert_eq!(lan_status, StatusCode::OK);

    // 各自拨各自的 host，各自用各自的协议与端口。
    assert!(lan.contains("server: \"198.51.100.1\"") && lan.contains("type: ss"), "{lan}");
    assert!(wan.contains("server: \"203.0.113.1\"") && wan.contains("type: vless"), "{wan}");
    assert!(!wan.contains("198.51.100.1"), "公网那份里不该有内网地址");
    assert!(!lan.contains("203.0.113.1"), "内网那份里不该有公网地址");
    assert!(!wan.contains("type: ss"), "公网那份里不该有 SS 节点：{wan}");
    assert!(!lan.contains("type: vless"), "内网那份里不该有 VLESS 节点：{lan}");
    // 端口按 +100 的规则各取各的。
    assert!(lan.contains("port: 65002") && lan.contains("port: 65003"), "{lan}");
    assert!(wan.contains("port: 65081") && wan.contains("port: 65082"), "{wan}");
    // **凭据也各取各的**：SS 那份是 `<iPSK>:<uPSK>`，VLESS 那份是 UUID。
    assert!(lan.contains("password: \"dGVzdC1pcHNr:"), "{lan}");
    assert!(wan.contains("uuid: \"00000000-0000-4000-8000-000000000001\""), "{wan}");
    // short-id 必须加引号：纯数字的十六进制不加引号会被 YAML 当成整数、
    // 前导零一并丢掉，而客户端对此只有一句「握手失败」。
    assert!(wan.contains("short-id: \"01234567\""), "short-id 没加引号：{wan}");

    // **入口名、代理组与规则两份逐字节相同。** 名字一变三个组与两份规则集
    // 模板都要跟着改，所以这一条是承重的。
    let tail =
        |body: &str| body[body.find("\nproxy-groups:").expect("应当有代理组段")..].to_string();
    assert_eq!(tail(&wan), tail(&lan), "proxy-groups 与 rules 两段必须逐字节相同");
    // 只数 proxies 段里的名字：代理组那一段也有 `- name:`，把它算进来的话
    // 这条断言就不再是在说入口了。
    let names = |body: &str| {
        let head = &body[..body.find("\nproxy-groups:").expect("应当有代理组段")];
        head.lines().filter(|l| l.starts_with("  - name: ")).map(str::to_string).collect::<Vec<_>>()
    };
    assert_eq!(names(&wan), names(&lan), "两份的入口名必须逐字相同且顺序一致");
    assert_eq!(names(&wan).len(), 2, "夹具里是两条入口");
}

/// **吊销一次，两条一起失效。** 它们共用同一个 token，这正是想要的。
#[tokio::test]
async fn 吊销对两种拨法同时生效() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.as_ref().unwrap().plaintext.clone();

    proxy_manager::subscription::revoke_for_user(&api.store, logged_in.user_id).await.unwrap();
    for prefix in ["proxyWan-", "proxyLan-"] {
        assert_eq!(
            api.get_text(&format!("/sub/{prefix}{token}.yaml"), None).await.0,
            StatusCode::NOT_FOUND,
            "{prefix} 吊销之后还能取到"
        );
    }
}

/// 认不出的前缀与认不出的 token 一样回 404——**不给探测接口**。
///
/// 也钉住老前缀 `Proxy-` 确实不再有效：它是一次有意的改动，
/// 不是「顺手还留着」。留着的话，同一份订阅会有三个地址，
/// 而第三个不在任何文档里。
#[tokio::test]
async fn 认不出的前缀回404() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let token = login(&api, "alice").await.subscription.unwrap().plaintext;

    for name in [
        format!("Proxy-{token}.yaml"),    // 旧前缀，已作废
        format!("proxyWAN-{token}.yaml"), // 大小写不同
        format!("proxy-{token}.yaml"),    // 更短
        format!("proxyWan{token}.yaml"),  // 少了连字符
        format!("proxyWan-{token}"),      // 缺扩展名
    ] {
        assert_eq!(
            api.get_text(&format!("/sub/{name}"), None).await.0,
            StatusCode::NOT_FOUND,
            "{name} 不该被接受"
        );
    }
}

/// **一个前缀不许是另一个的前缀。** 否则同一个文件名会匹配上两种拨法，
/// 而匹配到哪一个取决于配置顺序——一个静默的、改配置就会变的答案。
#[test]
fn 互为前缀的两种拨法会被配置校验挡住() {
    let toml = r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[sources]]
prefix = "proxy-"
host = "203.0.113.1"
transport = "ss"
[[sources]]
prefix = "proxy-lan-"
host = "198.51.100.1"
transport = "ss"
[[entries]]
name = "HK"
node_id = "n"
ports = { ss = 65002 }
[[groups]]
name = "手动选择"
proxies = ["HK"]
"#;
    let config: proxy_manager::config::SubscriptionConfig = toml::from_str(toml).unwrap();
    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("互为前缀"), "实际：{error}");
}

/// 前缀会进 URL 的最后一段：带 `/` 或 `.` 的前缀会让订阅地址指到别处，
/// 而那条路径**不认会话、只认 token**。
#[test]
fn 前缀里的路径字符会被挡住() {
    for bad in ["../", "a/b", "a.b", "带中文"] {
        let toml = format!(
            r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[sources]]
prefix = "{bad}"
host = "203.0.113.1"
transport = "ss"
[[entries]]
name = "HK"
node_id = "n"
ports = {{ ss = 65002 }}
[[groups]]
name = "手动选择"
proxies = ["HK"]
"#
        );
        let config: proxy_manager::config::SubscriptionConfig = toml::from_str(&toml).unwrap();
        let error = config.validate().unwrap_err().to_string();
        assert!(error.contains("只能是字母、数字与连字符"), "{bad} 没被挡住：{error}");
    }
}

// ============ VLESS 那一侧的配置门禁 ============

/// 改一处夹具配置再校验。返回错误文案。
fn reject(edit: impl Fn(&mut String)) -> String {
    let mut text = String::from(
        r#"
public_base_url = "https://pm.example.com"
credentials_path = "/tmp/creds.toml"
[[sources]]
prefix = "proxyLan-"
host = "198.51.100.1"
transport = "ss"
[[sources]]
prefix = "proxyWan-"
host = "203.0.113.1"
transport = "vless"
[[entries]]
name = "HK"
node_id = "n"
ports = { ss = 65002, vless = 65081 }
[[groups]]
name = "手动选择"
proxies = ["HK"]
[vless]
servername = "www.example.com"
client_fingerprint = "chrome"
flow = "xtls-rprx-vision"
packet_encoding = "xudp"
[vless.reality.n]
public_key = "cHVibGljLWtleQ"
short_id = "01234567"
"#,
    );
    edit(&mut text);
    let config: proxy_manager::config::SubscriptionConfig =
        toml::from_str(&text).expect("夹具本身必须解析得动");
    config.validate().unwrap_err().to_string()
}

/// 先证明没改过的那份是**通得过**的。少了这条，上面那些反向断言可能全是
/// 因为别的原因红的。
#[test]
fn 未改动的vless夹具能通过校验() {
    let config = fixture_config();
    config.validate().expect("夹具配置本身必须合法");
    assert_eq!(vless_source(&config).transport, proxy_manager::config::Transport::Vless);
    assert_eq!(ss_source(&config).transport, proxy_manager::config::Transport::Ss);
}

/// **少配一台节点的 REALITY 参数要启动就失败。**
///
/// 不挡的话发出去的是一份格式完全正确、却连不上的订阅，
/// 而客户端对此只有一句「握手失败」——指不到是主控少配了一台。
#[test]
fn 入口的落点没有reality参数会被挡住() {
    let error =
        reject(|text| *text = text.replace("[vless.reality.n]", "[vless.reality.another-node]"));
    assert!(error.contains("没有这一台"), "{error}");
    assert!(error.contains("HK"), "要点名是哪条入口：{error}");
}

/// 有 VLESS 拨法却没有 `[subscription.vless]`。
#[test]
fn 有vless来源却没有vless段会被挡住() {
    let error = reject(|text| {
        let cut = text.find("[vless]").expect("夹具里有这一段");
        text.truncate(cut);
    });
    assert!(error.contains("没有 [subscription.vless] 段"), "{error}");
}

/// 入口少了某种协议的端口。**这条的失败形态是「少了一个节点」**：
/// 那种拨法下这条入口会从订阅里消失，客户端不报错。
#[test]
fn 入口缺一种协议的端口会被挡住() {
    let error = reject(|text| *text = text.replace(", vless = 65081", ""));
    assert!(error.contains("没有 vless 端口"), "{error}");
    assert!(error.contains("proxyWan-"), "要点名是哪种拨法：{error}");

    let error = reject(|text| *text = text.replace("ss = 65002, ", ""));
    assert!(error.contains("没有 ss 端口"), "{error}");
}

/// VLESS 的公共参数有空字段——每一项都会原样进订阅正文。
#[test]
fn vless公共参数有空字段会被挡住() {
    for field in ["servername", "client_fingerprint", "flow", "packet_encoding"] {
        let error = reject(|text| {
            let line =
                text.lines().find(|l| l.starts_with(field)).expect("夹具里有这一行").to_string();
            *text = text.replace(&line, &format!("{field} = \"\""));
        });
        assert!(error.contains("有空字段"), "{field} 没被挡住：{error}");
    }
}

// ============ 凭据文件的 uuid 那一半 ============

fn credentials(upsk: &[&str], uuid: &[&str]) -> proxy_manager::config::Credentials {
    proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "dGVzdC1pcHNr".into(),
        upsk: upsk.iter().map(|n| (n.to_string(), "dXBzaw==".to_string())).collect(),
        uuid: uuid
            .iter()
            .map(|n| (n.to_string(), "00000000-0000-4000-8000-000000000001".to_string()))
            .collect(),
    }
}

/// **没有 uuid 的旧凭据文件仍然读得动。**
///
/// 这不是宽容，是推送顺序的保护：二进制与凭据是两次推送，
/// 「先二进制、后凭据」这一支靠的就是这个 `default`——那时 uuid 为空，
/// 公网订阅暂时发不出 VLESS，而**内网 SS 完全不受影响**。
/// 反过来（先凭据、后二进制）会因为 `deny_unknown_fields` 整份拒绝，
/// `/sub/…` 对所有人回 503。
#[test]
fn 没有uuid的旧凭据文件仍然合法() {
    let text = "method = \"m\"\nipsk = \"i\"\n[upsk]\nslot-01 = \"u\"\n";
    let parsed: proxy_manager::config::Credentials =
        toml::from_str(text).expect("旧文件必须还读得动");
    assert!(parsed.uuid.is_empty());
    parsed.validate().expect("整份没有 uuid 是合法的");
}

/// **「有一半」要被拒。**
///
/// 症状是**一部分人**的公网订阅悄悄没有节点——比所有人都没有难查得多。
#[test]
fn 凭据文件的uuid只有一半会被拒() {
    let error =
        credentials(&["slot-01", "slot-02"], &["slot-01"]).validate().unwrap_err().to_string();
    assert!(error.contains("不一致"), "{error}");
    assert!(error.contains("slot-02"), "要点名是谁缺的：{error}");

    // 反方向同样要拒：多出来的那个是谁的？
    let error =
        credentials(&["slot-01"], &["slot-01", "slot-09"]).validate().unwrap_err().to_string();
    assert!(error.contains("slot-09"), "{error}");

    // 正向控制：两边逐个对上就放行。
    credentials(&["slot-01", "slot-02"], &["slot-01", "slot-02"]).validate().unwrap();
}

/// **有 uPSK、没 UUID 的人在本人页上不能是绿的。**
///
/// 那正是这个项目定义的假绿：服务端知道公网那份订阅会是空配置，界面不说。
#[tokio::test]
async fn 只有upsk没有uuid时本人页说不可用() {
    let api = Api::with_subscription(&[], &["slot-01"]).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let alice = login(&api, "alice").await;

    // 先确认有 uuid 时是绿的——否则下面那条可能是因为别的原因红的。
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&alice))).await;
    assert_eq!(body["usable"], true, "{body}");

    // 把 uuid 那一半摘掉，模拟「二进制推了、凭据还没推」。
    api.rewrite_credentials(|text| {
        text.truncate(text.find("[uuid]").expect("夹具凭据里有这一段"));
    });
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(&actor(&alice))).await;
    assert_eq!(body["usable"], false, "{body}");
    assert_eq!(body["unusable_reason"], "no_credential", "{body}");
}

/// **`load` 取了几个端点，就得往 `render` 传几样。**
///
/// 少传一个的后果不是少一块内容，而是**整页报「加载失败：xxx is not defined」**——
/// `render` 解构的是 `load` 返回的那个对象，少一个键就取到 undefined，
/// 而它随后被当成对象用。页面上一个字都没有，看起来像服务端挂了。
///
/// 2026-09-20 给 me.html 加节点卡时就是这么炸的：`load` 里加了
/// `getJson("/me/usage")`，返回的却还是 `{ me, sub }`。
/// 上线之后才被使用者撞见——既有的用例验集合名、验绑定路径，
/// 唯独没有人数过这两边对不对得上。
#[test]
fn me页面取了几个端点就要传几样给render() {
    let (script, _) = proxy_manager::web::asset("assets/api.js").unwrap();
    let block = script
        .split("\"me.html\": {")
        .nth(1)
        .expect("api.js 里没有 me.html 那一段：扫描器坏了，或者结构变了");
    let load = block.split("render:").next().expect("me.html 那段里没有 render");

    let fetched = load.matches("getJson(").count();
    assert!(fetched >= 2, "只数出 {fetched} 个端点，扫描器多半坏了：{load}");

    let returned = load
        .rsplit("return {")
        .next()
        .and_then(|rest| rest.split('}').next())
        .expect("load 里没有 `return { … }`");
    let keys = returned.split(',').filter(|k| !k.trim().is_empty()).count();

    assert_eq!(
        keys, fetched,
        "me.html 的 load 取了 {fetched} 个端点，却只往 render 传了 {keys} 样：{returned:?}"
    );
}

/// **级别不够的节点整条入口不出现，而且组成员要跟着一起消失。**
///
/// 这一条守的是一个会让整份订阅报废的坑：代理组里写的是入口名，
/// 过滤掉入口却留着组员，Clash 会因为「组里引用了不存在的节点」
/// **拒绝整份配置**——低等级用户拿到的不是「少几个节点」，是完全不能用，
/// 而客户端只说一句「订阅格式错误」，指不到这里。
#[test]
fn 看不见的节点连同组成员一起消失() {
    let config = fixture_config();
    let credentials = proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "aaa".into(),
        upsk: [("slot-01".to_string(), "bbb".to_string())].into_iter().collect(),
        uuid: Default::default(),
    };
    // 夹具里只有一条入口，落在节点 "n" 上。把它挡在外面。
    let nothing = std::collections::BTreeSet::new();
    let yaml = proxy_manager::subscription::render::clash_yaml(
        &config,
        ss_source(&config),
        &credentials,
        "slot-01",
        "Lan-alice",
        &nothing,
        proxy_manager::subscription::custom::CustomNodes::NONE,
    );

    assert!(!yaml.contains("\"HK\""), "看不见的入口不该出现在 proxies 里：{yaml}");
    // **组里也不许再提它。** 这一条才是这个用例真正在守的东西。
    let groups = &yaml[yaml.find("\nproxy-groups:").expect("应当有代理组段")..];
    assert!(!groups.contains("HK"), "组成员没跟着过滤掉，整份配置会被客户端拒绝：{groups}");
    // 组不能变成空的——空组同样会让 Clash 拒绝整份配置。
    assert!(groups.contains("DIRECT"), "过滤之后组里至少要留下一个成员：{groups}");
}

/// 反向：级别够时一切照旧，一条都不少。
#[test]
fn 级别够时入口与组成员都在() {
    let config = fixture_config();
    let credentials = proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "aaa".into(),
        upsk: [("slot-01".to_string(), "bbb".to_string())].into_iter().collect(),
        uuid: Default::default(),
    };
    let all = std::collections::BTreeSet::from(["n"]);
    let yaml = proxy_manager::subscription::render::clash_yaml(
        &config,
        ss_source(&config),
        &credentials,
        "slot-01",
        "Lan-alice",
        &all,
        proxy_manager::subscription::custom::CustomNodes::NONE,
    );
    assert!(yaml.contains("  - name: \"HK\""), "{yaml}");
    let groups = &yaml[yaml.find("\nproxy-groups:").expect("应当有代理组段")..];
    assert!(groups.contains("HK"), "级别够就不该被过滤：{groups}");
}

/// **normal 看不到公网那份订阅地址，别的档位看得到。**
///
/// 注意这是**藏起来**，不是拦截：两种拨法共用同一个 token，把前缀从 `Lan-`
/// 改成 `Wan-` 照样取得到。使用者选的就是这个口径，所以这条用例只钉住
/// 「页面上给不给」——别把它读成访问控制的证据。
/// 本人页上列出来的拨法前缀，按 `sources` 的次序。
async fn 页上的拨法(api: &Api, actor: &crate::harness::Actor) -> Vec<String> {
    let (_, body, _) = api.get("/api/v1/me/subscription", Some(actor)).await;
    body["subscriptions"]
        .as_array()
        .unwrap_or_else(|| panic!("subscriptions 不是数组：{body}"))
        .iter()
        .map(|s| s["prefix"].as_str().unwrap().to_string())
        .collect()
}

#[tokio::test]
async fn 级别不够的拨法不出现在本人页上() {
    // 公网那份要 advanced（10）。新建的人一律落 normal。
    let api = Api::with_subscription_levels(&[], &["slot-01", "slot-02"], 10).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;

    // 先立控制组：**够格的人两条都看得见**。
    // 没有这一半，下面那条断言在「订阅整个坏了」时同样是绿的——
    // 本项目为这种假绿吃过亏（`unusable_reason` 那段注释记着现场）。
    let bob = login(&api, "bob").await;
    api.set_quota_group(bob.user_id, "advanced").await;
    let bob = actor(&bob);
    assert_eq!(页上的拨法(&api, &bob).await, ["proxyWan-", "proxyLan-"], "advanced 该看到两条");

    // 正题：normal 只剩内网那一条。
    let alice = actor(&login(&api, "alice").await);
    assert_eq!(页上的拨法(&api, &alice).await, ["proxyLan-"], "normal 不该看到公网那条");
}

/// 藏起来**不是**拦下来：同一个 token 换个前缀照样取得到公网那份。
///
/// 这条断言看着像在证明一个漏洞，它确实是——但这是使用者明确选的口径
/// （2026-09-20「只要隐藏就行」）。钉住它是为了让哪天改成真拦截时，
/// 这条用例**必须**跟着改，而不是让口径在无人察觉中漂移。
#[tokio::test]
async fn 藏起来的拨法换个前缀仍然取得到() {
    let api = Api::with_subscription_levels(&[], &["slot-01"], 10).await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    let logged_in = login(&api, "alice").await;
    let token = logged_in.subscription.as_ref().unwrap().plaintext.clone();
    let alice = actor(&logged_in);

    // 页面上只给内网那条。
    assert_eq!(页上的拨法(&api, &alice).await, ["proxyLan-"]);

    // 同一个 token，换个前缀——`/sub/` 那一侧不判级别。
    let (status, body, _) = api.get_text(&format!("/sub/proxyWan-{token}.yaml"), None).await;
    assert_eq!(status, StatusCode::OK, "换个前缀照样发：{body}");
    assert!(body.contains("type: vless"), "发出来的该是公网那份 VLESS：{body}");
}

/// 级别的判据本身：`normal` 够不到 10，其余三档都够。
#[test]
fn 只有normal够不到公网那一档() {
    use proxy_manager::quota::level;
    // 公网那份打算配 min_level = 10（= advanced）。
    assert!(level::of("normal") < 10, "normal 看不到");
    for group in ["advanced", "manage", "admin"] {
        assert!(level::of(group) >= 10, "{group} 应当看得到");
    }
}

// ============ 个人自定义节点进订阅 ============

use proxy_manager::custom::parse::{CustomProxy, CustomSocks5};
use proxy_manager::custom::store::{StoredProxy, StoredSocks5};
use proxy_manager::subscription::custom::CustomNodes;

fn stored_proxy(id: i64, share_url: &str, name: &str) -> StoredProxy {
    let config: CustomProxy = proxy_manager::custom::parse::parse_share_url(name, share_url)
        .unwrap_or_else(|error| panic!("夹具链接解析失败：{error}"));
    StoredProxy {
        id,
        protocol: config.protocol,
        config,
        created_at: "2026-09-22T00:00:00Z".into(),
        updated_at: "2026-09-22T00:00:00Z".into(),
    }
}

fn stored_socks(id: i64, name: &str, dialer: &str) -> StoredSocks5 {
    StoredSocks5 {
        id,
        config: CustomSocks5 {
            name: name.into(),
            server: "socks.example".into(),
            port: 1080,
            username: String::new(),
            password: String::new(),
            dialer_proxy: dialer.into(),
        },
        created_at: "2026-09-22T00:00:00Z".into(),
        updated_at: "2026-09-22T00:00:00Z".into(),
    }
}

fn render_with(custom: CustomNodes<'_>) -> String {
    let config = fixture_config();
    let credentials = proxy_manager::config::Credentials {
        method: "2022-blake3-aes-128-gcm".into(),
        ipsk: "aXBzaw==".into(),
        upsk: [("slot-01".to_string(), "dXBzaw==".to_string())].into_iter().collect(),
        uuid: [("slot-01".to_string(), "9ba335c9-0d3e-48a4-9f2e-28614c264a2d".to_string())]
            .into_iter()
            .collect(),
    };
    let visible = std::collections::BTreeSet::from(["n"]);
    proxy_manager::subscription::render::clash_yaml(
        &config,
        ss_source(&config),
        &credentials,
        "slot-01",
        "Lan-alice",
        &visible,
        custom,
    )
}

#[tokio::test]
async fn 自定义上游进订阅且带前缀() {
    let proxies = vec![stored_proxy(1, "ss://YWVzLTI1Ni1nY206cHc@up.example:8388", "家里")];
    let yaml = render_with(CustomNodes { proxies: &proxies, socks5: &[] });

    assert!(yaml.contains(r#"- name: "Custom-家里""#), "该带 Custom- 前缀：{yaml}");
    assert!(yaml.contains("server: \"up.example\""), "{yaml}");
    // **每个现有组都要能选到它。** 只进 proxies 不进组，用户会看到节点列在那儿
    // 却怎么也切不过去。
    let groups = &yaml[yaml.find("\nproxy-groups:").expect("应当有代理组段")..];
    assert!(groups.contains(r#"- "Custom-家里""#), "现有组里该出现：{groups}");
    // 受管入口一条都不能少。
    assert!(yaml.contains(r#"- name: "HK""#), "{yaml}");
}

/// **SOCKS5 的 dialer-proxy 指向自动生成的组，不是直接指向上游。**
///
/// 这一层是旧站最值钱的设计：前置节点成了客户端里可点的选项，
/// 用户换前置不用回控制台改配置、也不用重新导入订阅。
/// 去掉这一层会红——那正是要守的东西。
#[tokio::test]
async fn socks5带自动生成的拨号组() {
    let proxies = vec![stored_proxy(1, "ss://YWVzLTI1Ni1nY206cHc@up.example:8388", "家里")];
    let socks = vec![stored_socks(1, "落地", "家里")];
    let yaml = render_with(CustomNodes { proxies: &proxies, socks5: &socks });

    assert!(yaml.contains(r#"- name: "Socks5-落地""#), "{yaml}");
    assert!(
        yaml.contains(r#"dialer-proxy: "Socks5-落地-先选前置节点""#),
        "dialer 该指向自动生成的组而不是上游本身：{yaml}"
    );
    let groups = &yaml[yaml.find("\nproxy-groups:").expect("应当有代理组段")..];
    assert!(groups.contains(r#"- name: "Socks5-落地-先选前置节点""#), "拨号组该在：{groups}");

    // 组里第一项是用户选的那个（Clash 的 select 默认选第一项），
    // 其余上游与 DIRECT 跟在后面当备选。
    let start = groups.find(r#"- name: "Socks5-落地-先选前置节点""#).unwrap();
    let block = &groups[start..];
    let first = block.find(r#"- "Custom-家里""#).expect("选中的前置该在组里");
    let hk = block.find(r#"- "HK""#).expect("受管入口该是备选");
    let direct = block.find(r#"- "DIRECT""#).expect("DIRECT 该是备选");
    assert!(first < hk && first < direct, "用户选的那个必须排第一：{block}");
}

/// SOCKS5 的前置就是 DIRECT 时，组里第一项是 DIRECT。
#[tokio::test]
async fn socks5的前置可以是direct() {
    let socks = vec![stored_socks(1, "落地", "DIRECT")];
    let yaml = render_with(CustomNodes { proxies: &[], socks5: &socks });
    let groups = &yaml[yaml.find("\nproxy-groups:").expect("应当有代理组段")..];
    let block = &groups[groups.find(r#"- name: "Socks5-落地-先选前置节点""#).unwrap()..];
    let direct = block.find(r#"- "DIRECT""#).unwrap();
    let hk = block.find(r#"- "HK""#).unwrap();
    assert!(direct < hk, "选的是 DIRECT 就该排第一：{block}");
}

/// **兜底：重名时丢掉全部自定义节点，只发受管那部分，并在文件头说明。**
///
/// Clash 遇到重名会拒绝整份配置——用户看到的只有「订阅格式错误」。
/// 所以这里宁可少发几个节点，也不发一份整体不可用的。
#[tokio::test]
async fn 自定义节点与受管入口重名时整体丢弃并说明() {
    // 叫「HK」的自定义上游渲染后是 "Custom-HK"，不撞；
    // 真正会撞的是有人把自定义上游直接叫成受管组名的情况。这里构造后者。
    let proxies = vec![stored_proxy(1, "ss://YWVzLTI1Ni1nY206cHc@up.example:8388", "家里")];
    let dup = vec![
        stored_proxy(1, "ss://YWVzLTI1Ni1nY206cHc@up.example:8388", "家里"),
        stored_proxy(2, "ss://YWVzLTI1Ni1nY206cHc@other.example:8388", "家里"),
    ];
    // 先证明不重名时它是进得去的——否则下面那条断言可能是因为别的原因绿的。
    assert!(render_with(CustomNodes { proxies: &proxies, socks5: &[] }).contains("Custom-家里"));

    let yaml = render_with(CustomNodes { proxies: &dup, socks5: &[] });
    // 断的是「不作为节点出现」而不是「文本里没有这个词」——
    // 文件头那句说明里**会**带上冲突的名字，那是它该有的样子。
    assert!(!yaml.contains(r#"- name: "Custom-家里""#), "重名时该整体丢弃：{yaml}");
    assert!(yaml.contains("你的自定义节点这次没有发出来"), "要在文件头说明原因：{yaml}");
    // **受管节点不受影响**——这才是「宁可少发」的前提。
    assert!(yaml.contains(r#"- name: "HK""#), "受管入口不该被牵连：{yaml}");
}

/// 前置指向一个不存在的上游 → 悬空引用，同样整体丢弃。
#[tokio::test]
async fn socks5前置不存在时整体丢弃() {
    let socks = vec![stored_socks(1, "落地", "并不存在的上游")];
    let yaml = render_with(CustomNodes { proxies: &[], socks5: &socks });
    assert!(!yaml.contains(r#"- name: "Socks5-落地""#), "{yaml}");
    assert!(yaml.contains("你的自定义节点这次没有发出来"), "{yaml}");
    assert!(yaml.contains(r#"- name: "HK""#), "受管入口不该被牵连：{yaml}");
}

/// 没有自定义节点时，订阅与从前**逐字节相同**。
/// 这条守的是「加了这个功能不会悄悄改变所有人已有的订阅」。
#[tokio::test]
async fn 没有自定义节点时订阅不变() {
    let with_none = render_with(CustomNodes::NONE);
    let with_empty = render_with(CustomNodes { proxies: &[], socks5: &[] });
    assert_eq!(with_none, with_empty);
    assert!(!with_none.contains("Custom-"), "{with_none}");
    assert!(!with_none.contains("Socks5-"), "{with_none}");
    assert!(!with_none.contains("你的自定义节点"), "没有就不该有那句说明：{with_none}");
}

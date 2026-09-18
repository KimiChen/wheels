//! 一轮对抗式审查逐条落回来的回归用例。
//!
//! 每一条对应一个**真实存在过**的缺陷。放在一起是因为它们有共同的形状：
//! 全是「代码看起来对、注释也这么写、但实际行为相反」的那一类，
//! 而这一类在认证与授权路径上恰好是最贵的——它们不报错。

use axum::http::StatusCode;
use proxy_manager::api::session::Role;
use proxy_manager::sso::client::SsoIdentity;
use proxy_manager::sso::http::{self, Endpoint, HttpsError};
use proxy_manager::sso::roster::Roster;

use super::harness::Api;

fn identity(account: &str, fs_id: Option<&str>) -> SsoIdentity {
    SsoIdentity {
        account: account.to_string(),
        display_name: account.to_string(),
        feishu_fs_id: fs_id.map(str::to_string),
    }
}

// ============ 名单是热源，不是启动快照 ============

/// **改名单，下一个请求即生效——不重启进程。**
///
/// 原先 `Roster` 在 `Sso::new` 里构造一次就定型了，于是 D27 写的
/// 「每请求现解析」只做到了一半：求值确实是每请求的，但**源**是启动快照。
/// 结果是撤权必须重启进程，而 D27 花了整段篇幅论证那不可接受。
///
/// 这条用例只动磁盘上那一个文件，router、库、cookie 全都不动。
#[tokio::test]
async fn 改名单文件下一个请求就生效() {
    let api = Api::with_admins(&["boss"]).await;
    let actor = api.user("boss", Role::User, "paoyou").await;
    assert_eq!(api.get("/api/v1/nodes", Some(&actor)).await.0, StatusCode::OK);

    api.rewrite_roster(&[], &[]);
    assert_eq!(
        api.get("/api/v1/nodes", Some(&actor)).await.0,
        StatusCode::FORBIDDEN,
        "名单里没了就该掉权限，不等重启"
    );

    // 反向：加回来也立刻生效，不是只会单向失效。
    api.rewrite_roster(&["boss"], &[]);
    assert_eq!(api.get("/api/v1/nodes", Some(&actor)).await.0, StatusCode::OK);
}

/// 授权源**损坏时失败关闭**，不退回上一份好的名单。
///
/// §4.9 的原话是「授权配置缺失、损坏或非法 → fail closed，整个认证请求失败」。
/// 退回旧名单看起来更友好，含义却是：有人把某人从名单里删掉、同时手滑写坏了
/// 文件，于是那个人继续有权限。
#[tokio::test]
async fn 名单文件损坏时失败关闭() {
    let api = Api::with_admins(&["boss"]).await;
    let actor = api.user("boss", Role::Admin, "paoyou").await;
    assert_eq!(api.get("/api/v1/nodes", Some(&actor)).await.0, StatusCode::OK);

    api.corrupt_roster();
    let (status, body, _) = api.get("/api/v1/me", Some(&actor)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "读不出名单就不能放行任何人");
    assert_eq!(body["error"]["code"], "member_fact_stale");
}

// ============ 登录路径不得抹掉授权输入 ============

/// **上游返回空的飞书标识时，不得把库里已有的那个覆盖成 NULL。**
///
/// 名单可以按 `fs:<标识>` 匹配管理员，而接入说明写明「账号未关联用户或未设置
/// 飞书标识时返回空字符串」。无条件覆盖意味着上游某一次返回空值，
/// 这个人的匹配键就没了——下一个请求他不再是管理员，而且没有任何东西报错。
#[tokio::test]
async fn 上游返回空飞书标识时不覆盖已有的() {
    use sqlx::Row;
    let api = Api::with_roster(&["fs:boss-fs"], &[]).await;
    let roster =
        Roster::from_config(&toml_sso(&super::harness::server_toml_with(&["fs:boss-fs"], &[])));

    proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("boss", Some("boss-fs")),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    // 第二次登录，上游这次没给标识。
    let again = proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("boss", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert_eq!(again.role, Role::User, "这一次求值时上游没给标识，算不出管理员是对的");

    let stored: Option<String> = sqlx::query("SELECT feishu_fs_id FROM users WHERE login_name = ?")
        .bind("boss")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(stored.as_deref(), Some("boss-fs"), "库里那个标识不该被抹掉");
}

/// **SSO 登录不得吞掉同名的 break-glass 账号。**
///
/// `login_name` 全表唯一，而上游账号名是别人命名空间里的普通字符串。
/// 一个在泡游侧叫 `break-glass` 的账号登录一次，就会把本地应急管理员那一行
/// 改写掉：既继承它名下已开通的节点身份，又在没有任何审计记录的情况下
/// 把唯一的逃生通道摘掉。
#[tokio::test]
async fn sso登录不能吞掉本地应急账号() {
    use sqlx::Row;
    let api = Api::new().await;
    let local = api.user("break-glass", Role::Admin, "local").await;
    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET auth_provider = 'local' WHERE user_id = ?")
        .bind(local.user_id)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let error = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("break-glass", None),
        time::Duration::hours(8),
    )
    .await
    .expect_err("同名的上游账号不该拿到这一行");
    assert!(error.to_string().contains("本地应急账号"), "实际：{error}");

    let (role, provider): (String, Option<String>) =
        sqlx::query("SELECT role, auth_provider FROM users WHERE user_id = ?")
            .bind(local.user_id)
            .fetch_one(api.store.readers())
            .await
            .map(|row| (row.get(0), row.get(1)))
            .unwrap();
    assert_eq!(role, "admin", "逃生通道的角色不该被改");
    assert_eq!(provider.as_deref(), Some("local"), "归属不该被改");
}

/// 飞书标识撞唯一索引时，给一句能查的话，而不是一个「内部错误」。
#[tokio::test]
async fn 飞书标识重复时报得出原因() {
    let api = Api::new().await;
    proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", Some("same-fs")),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    let error = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("bob", Some("same-fs")),
        time::Duration::hours(8),
    )
    .await
    .expect_err("两个人不该共用一个飞书标识");
    assert!(error.to_string().contains("飞书标识与库里另一个用户重复"), "实际：{error}");
}

/// **`sso reconcile` 不得把 break-glass 管理员降成普通用户。**
///
/// 它不在名单里——名单那条路走不通正是它存在的场景。照单对齐等于
/// 用一条例行的运维命令拆掉逃生通道。
#[tokio::test]
async fn reconcile不碰本地应急账号() {
    let api = Api::new().await;
    let local = api.user("break-glass", Role::Admin, "local").await;
    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET auth_provider = 'local' WHERE user_id = ?")
        .bind(local.user_id)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let plan = proxy_manager::sso::plan_reconcile(&api.store, &Roster::empty()).await.unwrap();
    assert!(
        plan.iter().all(|r| r.login_name != "break-glass"),
        "本地应急账号不该出现在对齐名单里：{plan:?}"
    );
}

// ============ 手写协议栈的边界 ============

/// **恶意 chunk 长度行不得导致 panic。**
///
/// `size` 完全由对端给定。`ffffffffffffffff` 就是 `usize::MAX`：
/// release 下 `body.len() + size` 与 `size + 2` 都会回绕成小数，
/// 两道长度检查一起失效，随后 `&rest[..size]` 越界。
/// 一个约 30 字节的载荷就是一次远端可触发的拒绝服务。
#[test]
fn chunk长度溢出不会panic() {
    // **载荷必须先塞进一个非空的 chunk。**
    //
    // 只发一个 `ffffffffffffffff` 的 chunk 是抓不到这个 bug 的：
    // 那时 `body.len()` 是 0，`0 + usize::MAX` 不回绕，第一道检查照样判超限。
    // 要让两道检查一起失效，需要 `body.len() + size` **恰好**回绕：
    //   body.len() = 1，size = usize::MAX  →  和为 0，第一道检查过；
    //   size + 2 回绕成 1                  →  第二道检查也过；
    //   然后 `&rest[..usize::MAX]` 越界。
    let payload = "1\r\nA\r\nffffffffffffffff\r\nBBBB\r\n0\r\n\r\n";
    let raw = format!("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{payload}");
    let result = http::parse_response(raw.as_bytes());
    assert!(
        matches!(result, Err(HttpsError::TooLarge)),
        "约 40 字节就能触发的远端 panic：{result:?}"
    );

    // 附近的几个值一并钉住，防的是「只对 usize::MAX 特判」那种修法。
    for hex in ["fffffffffffffffe", "8000000000000000", "7fffffffffffffff", "100000"] {
        let payload = format!("1\r\nA\r\n{hex}\r\nBBBB\r\n0\r\n\r\n");
        let raw = format!("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{payload}");
        let result = http::parse_response(raw.as_bytes());
        assert!(result.is_err(), "长度行 {hex} 本不该成功：{result:?}");
    }

    // 反向：正常的多段 chunked 仍然解得开，别把修复做成「一律拒绝」。
    let good =
        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nA\r\n2\r\nBC\r\n0\r\n\r\n";
    let ok = http::parse_response(good.as_bytes()).unwrap();
    assert_eq!(ok.body, b"ABC");
}

/// `https://host?x=1` 不得把整段当成 authority。
///
/// 原先它会在启动校验时「合法」通过，要到第一个人点登录才在连接阶段炸掉——
/// 而那时报出来的是一个看不懂的 DNS 失败。
#[test]
fn 没有路径但带query的url解析正确() {
    let endpoint = Endpoint::parse("https://a.example.com?x=1").unwrap();
    assert_eq!(endpoint.host, "a.example.com");
    assert_eq!(endpoint.port, 443);
    assert_eq!(endpoint.target, "/?x=1");

    let fragment = Endpoint::parse("https://a.example.com#f").unwrap();
    assert_eq!(fragment.host, "a.example.com");
}

/// 控制字符检查必须真的被执行到。
///
/// 这条曾经是**假绿**：用例在 JSON 里塞的是一个**裸**控制字符，
/// serde_json 在解析阶段就拒了，`field()` 那条分支从未运行过——
/// 把检查整段删掉，用例照样通过。
///
/// 正确的造法是 JSON **转义**序列 ` `：它是合法 JSON，
/// 解码之后才是 NUL，于是能一路走到 `field()`。
#[test]
fn 转义的控制字符走得到字段检查() {
    let escaped = "\\u0000";
    let body =
        format!(r#"{{"code":0,"data":{{"account":"a{escaped}b","name":"","wxwork_id":""}}}}"#);
    // 先证明它是合法 JSON——否则这条用例又会变成假绿。
    let parsed: serde_json::Value = serde_json::from_str(&body).expect("必须是合法 JSON");
    assert_eq!(parsed["data"]["account"].as_str().unwrap().len(), 3);

    let error =
        proxy_manager::sso::client::parse_identity(200, "application/json", body.as_bytes())
            .unwrap_err();
    assert!(error.to_string().contains("控制字符"), "实际：{error}");
}

/// 非 ASCII 的不可见字符同样要挡：这些字段会进日志、进 HTML、进 CLI 表格。
#[test]
fn 不可见与改变阅读方向的字符被拒() {
    for escaped in ["\\u2028", "\\u202e", "\\u200b", "\\ufeff", "\\u0085"] {
        let body =
            format!(r#"{{"code":0,"data":{{"account":"a{escaped}b","name":"","wxwork_id":""}}}}"#);
        let result =
            proxy_manager::sso::client::parse_identity(200, "application/json", body.as_bytes());
        assert!(result.is_err(), "{escaped} 本不该通过：{result:?}");
    }
}

fn toml_sso(server_toml: &str) -> proxy_manager::config::SsoConfig {
    let parsed =
        proxy_manager::config::ServerConfig::from_str(server_toml, std::path::Path::new("<test>"))
            .unwrap();
    parsed.sso.unwrap()
}

//! 外呼硬化与 check-token 的响应形状（README §4.9 加固 5）。

use proxy_manager::sso::client::{self, CheckError};
use proxy_manager::sso::http::{self, Endpoint, HttpsError, MAX_RESPONSE_BYTES};

// ============ 端点解析：只走 HTTPS ============

#[test]
fn 只接受https() {
    assert!(Endpoint::parse("https://account.example.com/api/sso/check-token").is_ok());
    for bad in [
        "http://account.example.com/x",
        "//account.example.com/x",
        "account.example.com/x",
        "ftp://account.example.com/x",
        "https://",
    ] {
        assert!(matches!(Endpoint::parse(bad), Err(HttpsError::BadUrl(_))), "{bad:?} 本不该通过");
    }
}

/// **回环可以走 http，别的一律不行。**
///
/// 加固 5 的原话是「限定 HTTPS」，而那条规则的意图是**一次性 token 不得明文
/// 穿过网络**。到 `127.0.0.1` 的一跳不穿过任何网络，意图完整保留。
///
/// 需要它的理由是现场逼出来的：上游 `account.xmpaoyou.com` 只支持 TLS 1.2 的
/// **CBC** 套件，逐个试过 rustls/ring 的全部五个 AEAD 套件都被拒——
/// rustls 按设计不实现 CBC，所以它在任何配置下都连不上。
/// TLS 那一跳因此交给本机 nginx。
#[test]
fn 回环允许http而别的主机不允许() {
    let loopback = Endpoint::parse("http://127.0.0.1:8444/api/sso/check-token").unwrap();
    assert!(!loopback.tls, "回环这一跳不该走 TLS");
    assert_eq!(loopback.port, 8444);
    assert!(Endpoint::parse("http://localhost:8444/x").is_ok());
    assert!(Endpoint::parse("http://[::1]:8444/x").is_err(), "IPv6 字面量本就不接受");

    // **反向：任何非回环主机都必须是 https。** 这一条是整个放宽的边界，
    // 松掉它就等于把加固 5 整条删掉。
    for bad in [
        "http://account.xmpaoyou.com/api/sso/check-token",
        "http://10.0.0.1/x",
        "http://192.168.1.1:8444/x",
        "http://example.com/x",
    ] {
        let error = Endpoint::parse(bad).unwrap_err();
        assert!(matches!(error, HttpsError::BadUrl(_)), "{bad:?} 本不该通过");
        assert!(error.to_string().contains("明文"), "错误要说清为什么：{error}");
    }
    // https 仍然默认 443，http 默认 80——别把两个默认值搞混。
    assert_eq!(Endpoint::parse("https://a.example.com/x").unwrap().port, 443);
    assert!(Endpoint::parse("https://a.example.com/x").unwrap().tls);
}

/// `https://evil.com@real.com/` 这种写法在肉眼读配置时极易看反，
/// 而它在这条路径上没有任何正当用途。
#[test]
fn 拒绝userinfo() {
    assert!(matches!(
        Endpoint::parse("https://evil.example.com@real.example.com/x"),
        Err(HttpsError::BadUrl(_))
    ));
}

#[test]
fn 默认端口是443且能带端口与路径() {
    let plain = Endpoint::parse("https://a.example.com/api/x").unwrap();
    assert_eq!(
        (plain.host.as_str(), plain.port, plain.target.as_str()),
        ("a.example.com", 443, "/api/x")
    );

    let ported = Endpoint::parse("https://a.example.com:8443/api/x?y=1").unwrap();
    assert_eq!((ported.port, ported.target.as_str()), (8443, "/api/x?y=1"));

    // 没有路径时补 `/`：请求行里不能是空的。
    let bare = Endpoint::parse("https://a.example.com").unwrap();
    assert_eq!(bare.target, "/");
}

// ============ 表单编码：RFC3986，不是 `+` 代空格 ============

/// 生产实现用的是 `http_build_query(..., PHP_QUERY_RFC3986)`。
///
/// **`+` 必须编码成 `%2B`。** 用 form 编码的经典 `+` 代空格，会让一个含 `+` 的
/// token 在对端被解成空格——而那表现为「token 稳定地无效」，查不到根因。
#[test]
fn 表单编码走rfc3986() {
    assert_eq!(http::form_encode("token", "one-time-token+/="), "token=one-time-token%2B%2F%3D");
    assert_eq!(http::form_encode("token", "a b"), "token=a%20b");
    // unreserved 集合原样保留。
    assert_eq!(http::form_encode("token", "aZ0-._~"), "token=aZ0-._~");
}

// ============ 响应解析 ============

fn raw(head: &str, body: &str) -> Vec<u8> {
    format!("{head}\r\n\r\n{body}").into_bytes()
}

#[test]
fn 按content_length解析() {
    let body = r#"{"code":0}"#;
    let response = http::parse_response(&raw(
        &format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}",
            body.len()
        ),
        body,
    ))
    .unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(response.content_type, "application/json");
    assert_eq!(response.body, body.as_bytes());
}

/// 长度不符必须**先**失败：截断的 body 有可能仍然解析成一个合法 JSON。
#[test]
fn content_length不符即失败() {
    let error = http::parse_response(&raw(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 999",
        r#"{"code":0}"#,
    ))
    .unwrap_err();
    assert!(matches!(error, HttpsError::BadResponse(_)), "实际：{error:?}");
}

/// **chunked 必须认。** `Connection: close` 不阻止对端用 chunked，
/// 而那时 body 会带着十六进制长度行被当成 JSON——报出来的是「JSON 不合法」，
/// 与「token 无效」在日志里长得一模一样。
#[test]
fn 解得开chunked() {
    let response = http::parse_response(&raw(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked",
        "5\r\n{\"cod\r\n5\r\ne\":0}\r\n0\r\n\r\n",
    ))
    .unwrap();
    assert_eq!(response.body, br#"{"code":0}"#);
}

#[test]
fn chunked的长度行可以带扩展() {
    let response = http::parse_response(&raw(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked",
        "2;ext=1\r\n{}\r\n0\r\n\r\n",
    ))
    .unwrap();
    assert_eq!(response.body, b"{}");
}

#[test]
fn 截断的chunked被发现() {
    let error = http::parse_response(&raw(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked",
        "9\r\n{\"cod\r\n",
    ))
    .unwrap_err();
    assert!(matches!(error, HttpsError::BadResponse(_)), "实际：{error:?}");
}

/// 我们没请求过 gzip，出现即说明对端在做没预期的事。**宁可失败也不猜。**
#[test]
fn 不认识的传输编码直接失败() {
    let error = http::parse_response(&raw(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: gzip",
        "x",
    ))
    .unwrap_err();
    assert!(matches!(error, HttpsError::BadResponse(_)));
}

#[test]
fn 超过上限的响应被拒而不是截断() {
    let body = "x".repeat(MAX_RESPONSE_BYTES + 1);
    let error = http::parse_response(&raw(
        &format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}",
            body.len()
        ),
        &body,
    ))
    .unwrap_err();
    assert!(matches!(error, HttpsError::TooLarge), "实际：{error:?}");
}

// ============ check-token 的响应形状 ============

fn ok_body() -> Vec<u8> {
    br#"{"code":0,"message":"ok","data":{"phone":"13800138000","name":"Zhang","avatar":"https://e.example.com/a.jpg","account":"testuser","wxwork_id":"zhangsan_fs_id"}}"#.to_vec()
}

#[test]
fn 正常响应映射出身份() {
    let identity = client::parse_identity(200, "application/json", &ok_body()).unwrap();
    assert_eq!(identity.account, "testuser");
    assert_eq!(identity.display_name, "Zhang");
    assert_eq!(identity.feishu_fs_id.as_deref(), Some("zhangsan_fs_id"));
}

/// **失败时 `data` 是数组**，不是对象——接入说明给的失败样例就是 `"data":[]`。
///
/// 直接把 data 反序列化成结构体，会让「token 为空」这个正常的失败
/// 在解析阶段炸成一个看起来像上游故障的错误：告警会指向泡游，而问题在我们这边。
#[test]
fn 失败响应里data是数组也要认得出是token无效() {
    let body = br#"{"code":-1,"message":"token missing","data":[]}"#;
    let error = client::parse_identity(200, "application/json", body).unwrap_err();
    assert!(matches!(error, CheckError::InvalidToken), "实际：{error:?}");
}

/// 接入说明没承诺 `code` 是整数还是字符串。**两种都认**——
/// 在这里猜错的后果是把成功判成失败，没有人能登录。
#[test]
fn code的整数与字符串两种形态都认() {
    let numeric = br#"{"code":0,"data":{"account":"a","name":"","wxwork_id":""}}"#;
    assert!(client::parse_identity(200, "application/json", numeric).is_ok());
    let text = br#"{"code":"0","data":{"account":"a","name":"","wxwork_id":""}}"#;
    assert!(client::parse_identity(200, "application/json", text).is_ok());
    // 反向：别的值一律是失败，不能因为「非零就当零」而放行。
    let other = br#"{"code":1,"data":{"account":"a","name":"","wxwork_id":""}}"#;
    assert!(matches!(
        client::parse_identity(200, "application/json", other),
        Err(CheckError::InvalidToken)
    ));
}

/// 姓名为空时退回账号名：`display_name` 在库里是 NOT NULL，
/// 而一个空显示名在控制台上等于一行看不出是谁的记录。
#[test]
fn 姓名为空时退回账号名() {
    let body = br#"{"code":0,"data":{"account":"testuser","name":"","wxwork_id":""}}"#;
    let identity = client::parse_identity(200, "application/json", body).unwrap();
    assert_eq!(identity.display_name, "testuser");
    // 空的飞书标识规范化成 None，否则两个都没设的人会撞唯一索引。
    assert_eq!(identity.feishu_fs_id, None);
}

/// `code` 为 0 却没有 account，是上游在做我们没预期的事。
/// 判成**上游不可用**而不是 token 无效：前者会告警，后者只是让人重登一次。
#[test]
fn code为零但缺account判上游异常() {
    let body = br#"{"code":0,"data":{"account":"","name":"x","wxwork_id":""}}"#;
    let error = client::parse_identity(200, "application/json", body).unwrap_err();
    assert!(matches!(error, CheckError::Upstream(_)), "实际：{error:?}");
}

#[test]
fn 状态码与content_type都要对() {
    assert!(matches!(
        client::parse_identity(500, "application/json", &ok_body()),
        Err(CheckError::Upstream(_))
    ));
    assert!(matches!(
        client::parse_identity(200, "text/html", &ok_body()),
        Err(CheckError::Upstream(_))
    ));
    // 带 charset 的 content-type 要通过——上游实际就是这么发的。
    assert!(client::parse_identity(200, "application/json; charset=utf-8", &ok_body()).is_ok());
}

/// 逐字段限长并拒绝控制字符（加固 5）。
#[test]
fn 字段超长或含控制字符被拒() {
    let long = "a".repeat(300);
    let body = format!(r#"{{"code":0,"data":{{"account":"{long}","name":"","wxwork_id":""}}}}"#);
    assert!(matches!(
        client::parse_identity(200, "application/json", body.as_bytes()),
        Err(CheckError::Upstream(_))
    ));

    let control =
        format!(r#"{{"code":0,"data":{{"account":"a{}b","name":"","wxwork_id":""}}}}"#, r" ");
    assert!(matches!(
        client::parse_identity(200, "application/json", control.as_bytes()),
        Err(CheckError::Upstream(_))
    ));
}

/// 手机号与头像**不进身份**：长期档案只存 provider + external_id + 显示名。
/// 少存一个字段就少一份要在事故里解释的数据。
#[test]
fn 手机号与头像被丢弃() {
    let identity = client::parse_identity(200, "application/json", &ok_body()).unwrap();
    let rendered = format!("{identity:?}");
    assert!(!rendered.contains("13800138000"), "手机号不该出现在身份里：{rendered}");
    assert!(!rendered.contains("avatar"), "头像不该出现在身份里：{rendered}");
    assert!(!rendered.contains("a.jpg"), "头像不该出现在身份里：{rendered}");
}

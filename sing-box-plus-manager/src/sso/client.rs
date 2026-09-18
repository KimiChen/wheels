//! `POST /api/sso/check-token`：拿一次性 token 换身份。
//!
//! **content-type 是 `application/x-www-form-urlencoded`，不是 JSON。**
//!
//! 出处要说准：`operations` 仓的 `docs/paoyouProxy/sso.md` 步骤 6 **写明了**
//! 「该接口读取 `application/x-www-form-urlencoded` 表单，不读取 JSON 请求体」，
//! 而外发那一份接入说明（`~/Downloads/SSO功能.md`）**没有这一句**。
//! 也就是说这是一条被本方补记进文档的实测结论，不是「文档没写只能靠猜」。
//!
//! 仍然容易读反的是说明第 3 节那段 JavaScript 示例里的
//! `Content-Type: application/json`——它指的是浏览器打给**第三方自己**的回调端点，
//! 不是打给泡游的 check-token。照抄它会得到「token 不能为空」。
//!
//! 字段语义同理：上游那个字段名叫 `wxwork_id`（企业微信），装的却是**飞书标识**。
//! 本模块按实测语义命名为 `feishu_fs_id`，不继承上游的误名。

use std::time::Duration;

use serde::Deserialize;

use crate::sso::http::{self, Endpoint, HttpsError};

/// 单个字段的长度上限（README §4.9 加固 5：逐字段限长并拒绝控制字符）。
const MAX_FIELD_BYTES: usize = 256;
/// token 的长度上限。发请求**之前**就校验——一个明显不合法的 token
/// 不该换来一次外呼。
const MAX_TOKEN_BYTES: usize = 4096;

/// 换回来的身份。**只保留会长期留存的那几个字段。**
///
/// 上游还会返回 `phone` 与 `avatar`，这里明确丢弃：README §4.9 规定
/// 长期身份档案只存 provider + external_id + 显示名，不存手机号与头像 URL。
/// 少存一个字段就少一份要在事故里解释的数据。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SsoIdentity {
    /// 泡游游戏账号名。**大小写敏感**，也是建号的幂等键。
    pub account: String,
    /// 姓名。可能是空串（接入说明：账号未关联用户时返回空字符串）。
    pub display_name: String,
    /// 飞书标识。上游字段名是 `wxwork_id`，语义是 `fs_id`。空串规范化成 `None`。
    pub feishu_fs_id: Option<String>,
}

#[derive(Debug, thiserror::Error)]
pub enum CheckError {
    /// token 本身的问题：格式不对、上游判无效或已过期。
    #[error("token 无效或已过期")]
    InvalidToken,
    /// 上游不可用：连不上、超时、状态码或 content-type 不对、响应不可解析。
    ///
    /// **与 `InvalidToken` 分开。** 生产实现把两者收敛成同一个异常，
    /// 结果是「泡游挂了」和「你的链接过期了」在日志里长得一模一样。
    /// 对用户仍然统一跳转不回显原因（加固 5），但**日志和告警要能分**。
    #[error("上游不可用：{0}")]
    Upstream(String),
}

/// 上游的统一响应壳。
///
/// `data` 故意收成 [`serde_json::Value`]：接入说明给的失败样例是
/// `{"code":-1,"message":"token不能为空","data":[]}`——**失败时 `data` 是数组**。
/// 直接把它反序列化成结构体，会让「token 为空」这个正常的失败
/// 在解析阶段就炸成一个看起来像上游故障的错误。
#[derive(Deserialize)]
struct Envelope {
    /// 同样收成 `Value`：说明没承诺它是整数还是字符串。
    code: serde_json::Value,
    #[serde(default)]
    data: serde_json::Value,
}

#[derive(Deserialize)]
struct IdentityData {
    #[serde(default)]
    account: String,
    #[serde(default)]
    name: String,
    /// 上游的误名，只在这里出现一次，映射完就不再流通。
    #[serde(default)]
    wxwork_id: String,
}

/// 换取身份。
pub async fn check_token(
    endpoint: &Endpoint,
    token: &str,
    timeout: Duration,
) -> std::result::Result<SsoIdentity, CheckError> {
    // 发请求之前先把明显不合法的挡掉：省一次外呼，也少一次把垃圾发给上游。
    if token.is_empty()
        || token.len() > MAX_TOKEN_BYTES
        || token.bytes().any(|b| b < 0x20 || b == 0x7f)
    {
        return Err(CheckError::InvalidToken);
    }

    let body = http::form_encode("token", token);
    let response = http::post_form(endpoint, body, timeout).await.map_err(|e| match e {
        // URL 不合法是我们自己的配置错，但在这条路径上只能表现为上游不可用。
        HttpsError::BadUrl(detail) => CheckError::Upstream(format!("端点配置有误：{detail}")),
        other => CheckError::Upstream(other.to_string()),
    })?;
    parse_identity(response.status, &response.content_type, &response.body)
}

/// 把一份响应解析成身份。**纯函数**，与 I/O 分开。
///
/// 分开不是为了洁癖：这一段的分支全是「上游返回了什么形状」，
/// 而那些形状没有一种能在用例里经真实 TLS 造出来——
/// 对端要是 `account.xmpaoyou.com`，我们没有它的私钥。
/// 不拆出来，这些分支就只能靠读代码来相信。
pub fn parse_identity(
    status: u16,
    content_type: &str,
    body: &[u8],
) -> std::result::Result<SsoIdentity, CheckError> {
    // 三道形状门禁。它们**不是**接入说明承诺的（说明只承诺顶层 code），
    // 是生产实现加码的一层，已经在生产跑过。代价要知道：上游哪天换个反代
    // 返 201 或去掉 content-type，这里会整体失效——那时改的是这三行，
    // 而不是往下放松到「有 body 就解析」。
    if status != 200 {
        return Err(CheckError::Upstream(format!("状态码 {status}")));
    }
    if !content_type.starts_with("application/json") {
        return Err(CheckError::Upstream(format!("content-type 是 {content_type:?}")));
    }

    let envelope: Envelope = serde_json::from_slice(body)
        .map_err(|e| CheckError::Upstream(format!("响应不是合法 JSON：{e}")))?;

    // 顶层 code 必须是 0。整数与字符串两种形态都认——说明没承诺是哪一种，
    // 而在这里猜错的后果是**把成功判成失败**，没人能登录。
    let ok = match &envelope.code {
        serde_json::Value::Number(n) => n.as_i64() == Some(0),
        serde_json::Value::String(s) => s == "0",
        _ => false,
    };
    if !ok {
        // 不把 message 放进错误：它来自上游，会被原样记进日志。
        return Err(CheckError::InvalidToken);
    }

    let data: IdentityData = serde_json::from_value(envelope.data)
        .map_err(|e| CheckError::Upstream(format!("data 不是对象：{e}")))?;

    let account = field(&data.account, "account")?;
    if account.is_empty() {
        // 说明写明「校验成功时返回 account，账号不存在时不会返回成功响应」。
        // code 为 0 却没有 account，是上游在做我们没预期的事。
        return Err(CheckError::Upstream("code 为 0 但没有 account".into()));
    }
    let name = field(&data.name, "name")?;
    let fs_id = field(&data.wxwork_id, "wxwork_id")?;

    Ok(SsoIdentity {
        // 姓名为空时退回账号名：display_name 在库里是 NOT NULL，
        // 而一个空的显示名在控制台上等于一行看不出是谁的记录。
        display_name: if name.is_empty() { account.clone() } else { name },
        account,
        // **空串规范化成 None。** 库里那个唯一索引只跳过 NULL；
        // 留着空串会让两个都没设飞书标识的人在第二个人登录时撞唯一约束。
        feishu_fs_id: if fs_id.is_empty() { None } else { Some(fs_id) },
    })
}

/// 逐字段限长并拒绝控制字符（加固 5）。
///
/// 不只看 ASCII：这些字段会进日志、进控制台的 HTML、进 CLI 的表格。
/// `U+2028`/`U+2029` 在 JavaScript 源码里是行终止符，`U+0085` 在不少
/// 文本处理里同样算换行，bidi override 能让一个显示名在界面上**看起来**
/// 是另一个人的名字——而名单是逐字节比对的，肉眼核对会被骗。
fn field(value: &str, name: &'static str) -> std::result::Result<String, CheckError> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(CheckError::Upstream(format!("字段 {name} 超长")));
    }
    if value.bytes().any(|b| b < 0x20 || b == 0x7f) {
        return Err(CheckError::Upstream(format!("字段 {name} 含控制字符")));
    }
    if value.chars().any(is_dangerous_char) {
        return Err(CheckError::Upstream(format!("字段 {name} 含不可见或改变阅读方向的字符")));
    }
    Ok(value.to_string())
}

fn is_dangerous_char(c: char) -> bool {
    matches!(c,
        // C1 控制区与 NEL。
        '\u{80}'..='\u{9f}'
        // 行/段分隔符。
        | '\u{2028}' | '\u{2029}'
        // bidi override / embedding / isolate。
        | '\u{200e}' | '\u{200f}' | '\u{202a}'..='\u{202e}' | '\u{2066}'..='\u{2069}'
        // 零宽与 BOM：两个「不同」的名字可以长得一模一样。
        | '\u{200b}'..='\u{200d}' | '\u{feff}'
    )
}

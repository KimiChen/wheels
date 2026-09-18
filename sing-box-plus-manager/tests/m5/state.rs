//! `state`：形状、双提交绑定、TTL 与一次性墓碑（README §4.9 加固 1–3）。

use proxy_manager::config::StorageConfig;
use proxy_manager::sso::state::{self, StateError};
use proxy_manager::store::Store;
use proxy_manager_wire::sign::HmacKey;
use time::{Duration, OffsetDateTime};

const TTL: u32 = 300;

fn key() -> HmacKey {
    HmacKey::from_bytes(&[7u8; 32]).unwrap()
}

fn now() -> OffsetDateTime {
    OffsetDateTime::from_unix_timestamp(1_789_000_000).unwrap()
}

async fn store() -> (tempfile::TempDir, Store) {
    let dir = tempfile::tempdir().unwrap();
    let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
    let store = Store::open(&config).await.unwrap();
    store.init_schema().await.unwrap();
    (dir, store)
}

#[test]
fn 自己铸的state能过自己的校验() {
    let (issued, cookie) = state::issue(&key(), TTL, now());
    assert!(cookie.contains(state::STATE_COOKIE));
    let validated =
        state::validate(&key(), Some(&issued), Some(&issued), TTL, now()).expect("应当通过");
    assert_eq!(validated, issued);
}

/// **双提交**：query 里的 state 必须与回调专用 cookie 逐字节相等。
///
/// 这一条挡的是「攻击者把自己的回调链接发给受害者」——
/// 他能造出 query，但造不出受害者浏览器里的那个 cookie。
#[test]
fn cookie与query不一致就拒绝() {
    let (a, _) = state::issue(&key(), TTL, now());
    let (b, _) = state::issue(&key(), TTL, now());
    assert!(matches!(
        state::validate(&key(), Some(&a), Some(&b), TTL, now()),
        Err(StateError::Invalid)
    ));
    // 反向：缺任意一边同样拒绝，不能只有一边就放行。
    assert!(matches!(
        state::validate(&key(), Some(&a), None, TTL, now()),
        Err(StateError::Invalid)
    ));
    assert!(matches!(
        state::validate(&key(), None, Some(&a), TTL, now()),
        Err(StateError::Invalid)
    ));
}

#[test]
fn 换一把key就签不出同一个state() {
    let (issued, _) = state::issue(&key(), TTL, now());
    let other = HmacKey::from_bytes(&[9u8; 32]).unwrap();
    assert!(matches!(
        state::validate(&other, Some(&issued), Some(&issued), TTL, now()),
        Err(StateError::Invalid)
    ));
}

#[test]
fn 改一个字符就过不了() {
    let (issued, _) = state::issue(&key(), TTL, now());
    let mut bytes = issued.clone().into_bytes();
    // 翻随机量里的一位。签名不变，所以这是在验签名确实被检查了。
    let index = issued.find('.').unwrap() + 1;
    bytes[index] = if bytes[index] == b'a' { b'b' } else { b'a' };
    let tampered = String::from_utf8(bytes).unwrap();
    assert!(matches!(
        state::validate(&key(), Some(&tampered), Some(&tampered), TTL, now()),
        Err(StateError::Invalid)
    ));
}

#[test]
fn 过期的state被拒() {
    let (issued, _) = state::issue(&key(), TTL, now());
    let later = now() + Duration::seconds(TTL as i64 + 1);
    assert!(matches!(
        state::validate(&key(), Some(&issued), Some(&issued), TTL, later),
        Err(StateError::Expired)
    ));
    // 反向：刚好在边界上仍然有效——否则 TTL 的含义会悄悄少一秒。
    let edge = now() + Duration::seconds(TTL as i64);
    assert!(state::validate(&key(), Some(&issued), Some(&issued), TTL, edge).is_ok());
}

/// 允许小幅时钟前偏（加固 1），但不允许一个来自远未来的时间戳。
///
/// 前偏要允许的理由：签发与校验虽然在同一个进程里，NTP 步进与闰秒仍会造成
/// 几秒的倒退，为此拒绝一次登录不值当。
#[test]
fn 允许小幅前偏但拒绝远未来() {
    let (issued, _) = state::issue(&key(), TTL, now());
    let slightly_before = now() - Duration::seconds(20);
    assert!(state::validate(&key(), Some(&issued), Some(&issued), TTL, slightly_before).is_ok());

    let long_before = now() - Duration::seconds(120);
    assert!(matches!(
        state::validate(&key(), Some(&issued), Some(&issued), TTL, long_before),
        Err(StateError::Invalid)
    ));
}

/// 严格的形状检查。挡的是「构造一个自己的串去撞签名」之前的那一层。
#[test]
fn 形状不对的一律拒绝() {
    let k = key();
    let (good, _) = state::issue(&k, TTL, now());
    let mut parts = good.split('.');
    let (issued, random, mac) =
        (parts.next().unwrap(), parts.next().unwrap(), parts.next().unwrap());

    let bad = [
        String::new(),
        "no-dots".to_string(),
        format!("{issued}.{random}"),                   // 少一段
        format!("{issued}.{random}.{mac}.extra"),       // 多一段
        format!("{issued}.{}.{mac}", &random[..63]),    // 随机量短一位
        format!("{issued}.{random}.{}", &mac[..63]),    // 签名短一位
        format!("{issued}.{}z.{mac}", &random[..63]),   // 非十六进制
        format!("-1.{random}.{mac}"),                   // 负时间戳
        format!("12345678901234567890.{random}.{mac}"), // 时间戳超长
        format!(".{random}.{mac}"),                     // 空时间戳
    ];
    for candidate in bad {
        assert!(
            state::validate(&k, Some(&candidate), Some(&candidate), TTL, now()).is_err(),
            "这个串本不该被接受：{candidate:?}"
        );
    }
}

/// 墓碑是一次性的：同一个 state 第二次就是重放。
#[tokio::test]
async fn 同一个state只能消费一次() {
    let (_dir, store) = store().await;
    let (issued, _) = state::issue(&key(), TTL, now());
    assert!(state::consume(&store, &issued, TTL).await.unwrap(), "第一次应当成功");
    assert!(!state::consume(&store, &issued, TTL).await.unwrap(), "第二次必须判重放");
}

/// 过期墓碑在下一次消费时被顺手清掉——这张表因此不会无限长，
/// 也就不需要另起一个定时清理任务。
#[tokio::test]
async fn 过期墓碑会被清理() {
    use sqlx::Row;
    let (_dir, store) = store().await;
    let (old, _) = state::issue(&key(), TTL, now());
    // TTL 为 0：写进去的墓碑立刻就是过期的。
    assert!(state::consume(&store, &old, 0).await.unwrap());

    let (fresh, _) = state::issue(&key(), TTL, now());
    assert!(state::consume(&store, &fresh, TTL).await.unwrap());

    let count: i64 = sqlx::query("SELECT count(*) FROM sso_used_states")
        .fetch_one(store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 1, "过期的那条应当已被清掉，只剩新鲜的一条");
}

/// 回调 cookie **必须**用 `__Host-` 前缀。
///
/// 这条曾经写反：当时断言的是 `__Secure-` + `Path=/auth/sso/callback`，
/// 理由是「加固 2 要求把作用域收在回调路径上」。那个取舍换错了东西——
/// `Path` 不是写入隔离边界（浏览器从不按 path 隔离 cookie 的**写入**，
/// 而它已经是 HttpOnly，页面脚本本来就读不到），而 `__Host-` 才是唯一能阻止
/// **兄弟子域写入同名 cookie** 的机制。
///
/// 被架空的正是这套防线的全部理由：「攻击者能造出 query，但造不出受害者
/// 浏览器里的那个 cookie」。`__Secure-` 只校验 `Secure` 标志，不约束 `Domain`，
/// 于是控制台域名下任意一个攻击者可写的子域都能下发同名 cookie，
/// 把双提交变成一次登录 CSRF——受害者被固定进攻击者的账号。
#[test]
fn 回调cookie必须是host前缀() {
    let (_, cookie) = state::issue(&key(), TTL, now());
    assert!(cookie.starts_with("__Host-"), "实际：{cookie}");
    // `__Host-` 的三条硬性要求，缺一条浏览器就整个丢弃这个 cookie。
    assert!(cookie.contains("Path=/;") || cookie.contains("Path=/ "), "必须 Path=/：{cookie}");
    assert!(cookie.contains("Secure"), "实际：{cookie}");
    assert!(!cookie.contains("Domain="), "__Host- 不允许 Domain：{cookie}");

    assert!(cookie.contains("HttpOnly"), "页面脚本没有任何读它的理由");
    // SameSite=Lax 而不是 Strict：回调是上游发起的顶层导航，
    // Strict 会让这个 cookie 根本发不出来，于是登录必然失败。
    assert!(cookie.contains("SameSite=Lax"), "实际：{cookie}");
}

#[test]
fn 清cookie的头会让浏览器立刻丢掉它() {
    let cleared = state::clear_cookie();
    assert!(cleared.starts_with("__Host-"), "清除用的属性必须与下发时一致，否则清不掉");
    assert!(cleared.contains("Max-Age=0"));
    assert!(cleared.contains("Expires=Thu, 01 Jan 1970"));
    assert!(cleared.contains("Path=/"));
}

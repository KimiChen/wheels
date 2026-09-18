//! 名单是授权的事实来源（D27），以及建号事务。
//!
//! 这一组里最关键的是「改名单立即生效」与「`users.role` 不提权」两条。
//! 它们互为反面：前者证明名单被读了，后者证明库里那一列**没有**被读。
//! 只写前一条是不够的——一份同时读两处、取其大者的实现也能让它变绿。

use axum::http::StatusCode;
use proxy_manager::api::session::{self, Role};
use proxy_manager::config::SsoConfig;
use proxy_manager::sso::client::SsoIdentity;
use proxy_manager::sso::roster::Roster;

use super::harness::Api;

fn sso_config(toml_body: &str) -> SsoConfig {
    let toml = format!(
        "login_url = \"https://account.example.com/sso\"\n\
         check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
         public_base_url = \"https://pm.example.com\"\n{toml_body}"
    );
    let config: SsoConfig = toml::from_str(&toml).unwrap();
    config.validate().unwrap();
    config
}

fn identity(account: &str, fs_id: Option<&str>) -> SsoIdentity {
    SsoIdentity {
        account: account.to_string(),
        display_name: account.to_string(),
        feishu_fs_id: fs_id.map(str::to_string),
    }
}

// ============ 名单求值 ============

/// 名单里没有的人是**普通用户**，不是被拒绝，也不是管理员。
///
/// 空名单最容易被误当成「还没配，先都放行」——认证代码的失败模式
/// 正是沉默地放行，所以这一条要钉死。
#[test]
fn 空名单里所有人都是普通用户() {
    let roster = Roster::empty();
    let grade = roster.grade("anyone", Some("fs-1"));
    assert_eq!(grade.role, Role::User);
    assert_eq!(grade.quota_group, "normal");
    assert_eq!(roster.admin_count(), 0);
}

#[test]
fn 按账号名与飞书标识两种键都能命中() {
    let roster = Roster::from_config(&sso_config(
        "admins = [\"boss\", \"fs:fs-of-vp\"]\n\
         [quota_groups]\n\
         manage = [\"lead\"]\n\
         advanced = [\"fs:fs-of-dev\"]\n",
    ));
    assert_eq!(roster.grade("boss", None).role, Role::Admin);
    assert_eq!(roster.grade("someone", Some("fs-of-vp")).role, Role::Admin);
    assert_eq!(roster.grade("lead", None).quota_group, "manage");
    assert_eq!(roster.grade("whoever", Some("fs-of-dev")).quota_group, "advanced");
    // 反向：没命中的仍是普通用户 + normal。
    assert_eq!(roster.grade("nobody", Some("fs-nobody")).role, Role::User);
    assert_eq!(roster.grade("nobody", None).quota_group, "normal");
}

/// **大小写敏感**（README §4.9）。这不是细节：名单是逐字节比较的，
/// 而「大小写写错了」与「查无此人」在界面上长得一模一样。
#[test]
fn 名单大小写敏感() {
    let roster = Roster::from_config(&sso_config("admins = [\"Boss\"]\n"));
    assert_eq!(roster.grade("Boss", None).role, Role::Admin);
    assert_eq!(roster.grade("boss", None).role, Role::User, "大小写不同就不该命中");
}

/// 两种键都写了的时候，写得更具体的那条说了算。
#[test]
fn 账号名优先于飞书标识() {
    let roster = Roster::from_config(&sso_config(
        "[quota_groups]\n\
         manage = [\"alice\"]\n\
         advanced = [\"fs:alice-fs\"]\n",
    ));
    assert_eq!(roster.grade("alice", Some("alice-fs")).quota_group, "manage");
}

// ============ 配置校验 ============

fn rejects(toml_body: &str) -> String {
    let toml = format!(
        "login_url = \"https://account.example.com/sso\"\n\
         check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
         public_base_url = \"https://pm.example.com\"\n{toml_body}"
    );
    let config: SsoConfig = toml::from_str(&toml).expect("TOML 本身应当能解析");
    config.validate().expect_err("这份配置本不该通过").to_string()
}

/// 写错一个档位名的后果是**那一组人静默落回 normal**：没有报错，
/// 只有额度对不上，而那要一个月才看得出来。
#[test]
fn 档位名写错会被挡住() {
    let error = rejects("[quota_groups]\nsuper = [\"a\"]\n");
    assert!(error.contains("super"), "实际：{error}");
}

/// 同一个人在两档时「按哪一档算」没有答案，而「按额度高的算」
/// 和「按声明顺序算」都是悄悄替使用者做决定。
#[test]
fn 一个人不得同时出现在两档() {
    let error = rejects("[quota_groups]\nmanage = [\"a\"]\nadvanced = [\"a\"]\n");
    assert!(error.contains("多个额度档位"), "实际：{error}");
}

#[test]
fn 非https的端点被挡住() {
    let toml = "login_url = \"http://account.example.com/sso\"\n\
                check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
                public_base_url = \"https://pm.example.com\"\n";
    let config: SsoConfig = toml::from_str(toml).unwrap();
    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("login_url"), "实际：{error}");
}

/// 名单是逐字节比较的，一个尾随空格肉眼看不出来，但它让条目永远命不中。
#[test]
fn 名单条目里的空白被挡住() {
    let error = rejects("admins = [\"boss \"]\n");
    assert!(error.contains("空白"), "实际：{error}");
}

/// `public_base_url` 带路径段时启动就要失败。
///
/// 回调路由挂在**绝对路径** `/auth/sso/callback` 上，而回调地址是
/// `<基址>/auth/sso/callback`——基址带了前缀，泡游跳回来就路由不到，
/// 表现是登录 100% 失败且**没有任何诊断**：请求根本没进到回调 handler，
/// 主控这边连一条日志都不会有。
#[test]
fn 基址不能带路径段() {
    let error = rejects_base("https://pm.example.com/console");
    assert!(error.contains("不能带路径段"), "实际：{error}");
    // 反向：单个尾斜杠是常见写法，不该被拒。
    let toml = format!(
        "login_url = \"https://account.example.com/sso\"\n\
         check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
         public_base_url = \"{}\"\n",
        "https://pm.example.com/"
    );
    let config: SsoConfig = toml::from_str(&toml).unwrap();
    assert!(config.validate().is_ok(), "尾斜杠应当被接受");
}

fn rejects_base(base: &str) -> String {
    let toml = format!(
        "login_url = \"https://account.example.com/sso\"\n\
         check_token_url = \"https://account.example.com/api/sso/check-token\"\n\
         public_base_url = \"{base}\"\n"
    );
    let config: SsoConfig = toml::from_str(&toml).unwrap();
    config.validate().expect_err("这份配置本不该通过").to_string()
}

#[test]
fn 回调地址由基址拼出() {
    let config = sso_config("");
    assert_eq!(config.callback_url(), "https://pm.example.com/auth/sso/callback");
    // 尾斜杠不该产生双斜杠。
    let mut with_slash = config.clone();
    with_slash.public_base_url = "https://pm.example.com/".into();
    assert_eq!(with_slash.callback_url(), "https://pm.example.com/auth/sso/callback");
}

/// `url` 参数必须**整体**编码：回调地址自己带着 `?state=`，
/// 不编码的话那个 `?` 会把上游的参数结构劈开（接入说明第 5 节）。
#[test]
fn 跳转地址把回调整体编码() {
    let sso = proxy_manager::sso::Sso::new(&sso_config("")).unwrap();
    let location = sso.login_location("1789.abc.def");
    assert!(location.starts_with("https://account.example.com/sso?url="), "实际：{location}");
    assert!(location.contains("%3Fstate%3D"), "回调里的 ? 与 = 必须被编码：{location}");
    assert!(!location[location.find("url=").unwrap()..].contains('?'), "编码后不该再有裸 ?");
}

// ============ 建号事务 ============

#[tokio::test]
async fn 首次登录建号并按名单落档() {
    use sqlx::Row;
    let api = Api::new().await;
    let roster = Roster::from_config(&sso_config(
        "admins = [\"boss\"]\n[quota_groups]\nmanage = [\"boss\"]\n",
    ));
    let logged_in = proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("boss", Some("boss-fs")),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert_eq!(logged_in.role, Role::Admin);
    assert_eq!(logged_in.quota_group, "manage");

    let row = sqlx::query(
        "SELECT quota_group, role, auth_provider, feishu_fs_id FROM users WHERE login_name = ?",
    )
    .bind("boss")
    .fetch_one(api.store.readers())
    .await
    .unwrap();
    assert_eq!(row.get::<String, _>(0), "manage");
    assert_eq!(row.get::<String, _>(1), "admin");
    assert_eq!(row.get::<String, _>(2), "paoyou");
    assert_eq!(row.get::<Option<String>, _>(3).as_deref(), Some("boss-fs"));
}

/// 重复登录**不重复建号**，幂等键是账号名。
#[tokio::test]
async fn 重复登录幂等() {
    use sqlx::Row;
    let api = Api::new().await;
    let roster = Roster::empty();
    let first = proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    let second = proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert_eq!(first.user_id, second.user_id);
    let count: i64 = sqlx::query("SELECT count(*) FROM users")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 1);
}

/// 两个都没设飞书标识的人必须能各自建号。
///
/// 空串若原样落库，第二个人会撞上 `feishu_fs_id` 的唯一索引——
/// 而那表现为「某些人登录时 500」，且取决于谁先登录，极难复现。
#[tokio::test]
async fn 两个没有飞书标识的人都能建号() {
    let api = Api::new().await;
    let roster = Roster::empty();
    for account in ["alice", "bob"] {
        proxy_manager::sso::complete_login(
            &api.store,
            &roster,
            &identity(account, None),
            time::Duration::hours(8),
        )
        .await
        .unwrap_or_else(|e| panic!("{account} 建号失败：{e}"));
    }
}

/// 停用的账号**不得**靠重新登录复活。
///
/// 停用是管理员的显式动作，而登录路径没有任何东西能判断那个动作该不该撤销。
#[tokio::test]
async fn 停用的账号不能靠重新登录复活() {
    let api = Api::new().await;
    let roster = Roster::empty();
    proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET status = 'disabled' WHERE login_name = 'alice'")
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let error = proxy_manager::sso::complete_login(
        &api.store,
        &roster,
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .expect_err("停用的账号不该登得进来");
    assert!(error.to_string().contains("已停用"), "实际：{error}");
}

/// 名单里的档位与库里不一致时**只报出来，不在登录路径上改**。
///
/// 改额度是一次影响他人的操作（D21）：要留操作者、要预览确认、
/// 要生成逐节点下发任务。那一整套不该由某个人碰巧登录了一次来触发。
#[tokio::test]
async fn 档位漂移被报出来但不在登录路径上改() {
    use sqlx::Row;
    let api = Api::new().await;
    proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    let promoted = Roster::from_config(&sso_config("[quota_groups]\nmanage = [\"alice\"]\n"));
    let again = proxy_manager::sso::complete_login(
        &api.store,
        &promoted,
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert_eq!(again.quota_group_changed_from.as_deref(), Some("normal"));

    let stored: String = sqlx::query("SELECT quota_group FROM users WHERE login_name = 'alice'")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(stored, "normal", "登录路径不得改档位，那要走审计");

    // 但 reconcile 要能看见这件事。
    let plan = proxy_manager::sso::plan_reconcile(&api.store, &promoted).await.unwrap();
    assert_eq!(plan.len(), 1);
    assert!(plan[0].group_changed());
    assert_eq!(plan[0].to_group, "manage");
}

// ============ 每请求求值：这一组是整套设计的核心 ============

/// **改名单，下一个请求立即生效。**
#[tokio::test]
async fn 名单里加上管理员之后下一个请求就生效() {
    let api = Api::with_admins(&["boss"]).await;
    // 库里写的是 user——名单说了算。
    let actor = api.user("boss", Role::User, "paoyou").await;
    assert_eq!(
        api.get("/api/v1/nodes", Some(&actor)).await.0,
        StatusCode::OK,
        "名单里是管理员，库里那一列不该拦住他"
    );
}

/// **反向证据：改 `users.role` 不提权。**
///
/// 没有这一条，一份「同时读名单和库、取其大者」的实现也能让上一条变绿，
/// 而那种实现意味着改一行库就能提权。
#[tokio::test]
async fn 改库里的role不能提权() {
    let api = Api::new().await; // 空名单
    let actor = api.user("sneaky", Role::Admin, "paoyou").await;
    let (status, body, _) = api.get("/api/v1/nodes", Some(&actor)).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "users.role 是展示缓存，不是授权依据");
    assert_eq!(body["error"]["code"], "forbidden");
}

/// 从名单里删掉一个人，他的既有会话**下一个请求**就掉权限。
#[tokio::test]
async fn 从名单里删掉的人立刻掉权限() {
    let api = Api::with_admins(&["boss"]).await;
    let actor = api.user("boss", Role::Admin, "paoyou").await;
    assert_eq!(api.get("/api/v1/nodes", Some(&actor)).await.0, StatusCode::OK);

    // **只改磁盘上那个文件。** router、库、cookie 全都不动——
    // 这才是「改名单下一个请求即生效」的真实现场。
    api.rewrite_roster(&["someone-else"], &[]);
    let (status, _, _) = api.get("/api/v1/nodes", Some(&actor)).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "名单里没了就该立刻掉权限，不等重启也不等下次登录");
}

/// break-glass 是唯一的例外：它不在名单里，存在的理由正是名单那条路走不通。
#[tokio::test]
async fn break_glass仍然读库里的role() {
    let api = Api::new().await; // 空名单
    let actor = api.user("break-glass", Role::Admin, "local").await;
    assert_eq!(
        api.get("/api/v1/nodes", Some(&actor)).await.0,
        StatusCode::OK,
        "local 不在名单里，它的角色只能来自库"
    );
}

/// 泡游会话的成员事实窗口 == 会话窗口。
///
/// 泡游只有一次性的 check-token，**没有按账号查状态的接口**，
/// 所以那个窗口续不了。若照 D13 的 60 秒配，每个人登录一分钟后就会
/// 集体 401，而页面层把错误吞成「跳登录页」，症状是「一直莫名其妙被踢」。
#[tokio::test]
async fn 泡游会话不会在一分钟后掉线() {
    use sqlx::Row;
    let api = Api::new().await;
    let actor = api.user("alice", Role::User, "paoyou").await;
    let row =
        sqlx::query("SELECT member_fact_expires_at, expires_at FROM sessions WHERE session_pk = ?")
            .bind(actor.session_pk)
            .fetch_one(api.store.readers())
            .await
            .unwrap();
    assert_eq!(
        row.get::<Option<String>, _>(0).as_deref(),
        Some(row.get::<String, _>(1).as_str()),
        "泡游没有可刷新的成员事实源，窗口只能等于会话本身（D27）"
    );

    // 反向：手工把它过期之后仍然要拒——这条闸门本身没有被拆掉。
    session::expire_member_fact(&api.store, actor.session_pk).await.unwrap();
    let (status, body, _) = api.get("/api/v1/me", Some(&actor)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(body["error"]["code"], "member_fact_stale");
}

// ============ 首次登录自动领取（D11、README §4.9） ============

/// **登录即领取。** 没有单独的「开通」步骤。
///
/// 这条曾经被刻意做成两步，理由是「开通要留操作者、要落审计、可能失败」。
/// 三件事都成立，但没有一件需要一个**人**来按按钮——操作者记成 `sso:<账号>`，
/// 审计照样落，池空照样是错误。而拆成两步的代价是：每个新人在管理员跑完
/// 那条命令之前，登录进来看到的是一个什么都用不了的控制台。
#[tokio::test]
async fn 首次登录就领到槽位() {
    use sqlx::Row;
    let api = Api::new().await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;

    let logged_in = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    let claim = logged_in.claim.expect("登录应当顺带领到一个槽位");
    assert!(claim.newly_claimed);
    assert_eq!(claim.nodes, vec!["node-a".to_string()]);

    // 槽位真的落到了这个人头上，而不是只在返回值里。
    let (name, state): (String, String) =
        sqlx::query("SELECT identity_name, state FROM identity_routes WHERE user_id = ?")
            .bind(logged_in.user_id)
            .fetch_one(api.store.readers())
            .await
            .map(|row| (row.get(0), row.get(1)))
            .unwrap();
    assert_eq!(name, claim.identity_name);
    assert_eq!(state, "claimed");
}

/// 重复登录**不再消耗一个名额**，拿回同一个身份。
///
/// 池子只减不增（退役永不复用），所以「每次登录领一个」会在第 301 次登录时
/// 把池子掏空——而那看起来像一次容量规划失误，不像一个幂等 bug。
#[tokio::test]
async fn 重复登录不再消耗名额() {
    use sqlx::Row;
    let api = Api::new().await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;

    let first = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    let second = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();

    let a = first.claim.unwrap();
    let b = second.claim.unwrap();
    assert_eq!(a.identity_name, b.identity_name, "应当拿回同一个身份");
    assert!(a.newly_claimed && !b.newly_claimed, "第二次不是新领");

    let claimed: i64 = sqlx::query("SELECT count(*) FROM identity_routes WHERE state = 'claimed'")
        .fetch_one(api.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(claimed, 1, "只该占用一个名额");
}

/// **池空不让登录失败。**
///
/// 这是这一组里最重要的一条。领取是一次**容量**操作，而容量问题不该表现为
/// 「登不进去」——反过来做，一次可恢复的池耗尽会变成全体彻底进不来，
/// 连看一眼自己状态、连管理员进去扩容都做不到。
#[tokio::test]
async fn 池空时登录照常成功只是没有身份() {
    let api = Api::new().await;
    // 一个节点、一个槽位：第二个人必然领不到。
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;

    let first = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert!(first.claim.is_some());

    let second = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("bob", None),
        time::Duration::hours(8),
    )
    .await
    .expect("池空**不得**让登录失败");
    assert!(second.claim.is_none(), "没领到就是 None，不是一个假的成功");
    // 反向证据：他确实登进来了——会话是真的。
    assert!(!second.session.cookie_value.is_empty());
}

/// 用过的身份不得被分配出去（D11 的零基线门禁）。
///
/// 挡的是「把一个用过的身份发给新用户，于是他一上来就欠着别人的账」。
#[tokio::test]
async fn 有过流量的身份不会被领走() {
    let api = Api::new().await;
    api.seed_claimable_pool(&["node-a"], &["slot-01"]).await;
    api.dirty_slot("node-a", "slot-01").await;

    let logged_in = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert!(logged_in.claim.is_none(), "唯一的候选身份动过字节，不该被领走");
}

/// **从未认领的身份也能退役**，而且这是主要用途之一。
///
/// 部署侧的测试身份（`deploy-test`）、原型控制器的测试账号（`user0001`）
/// 都在节点的 inbound 里，但它们的凭据在别处流通——发给真人等于两个人
/// 共用一份凭据。在这条路存在之前，排除它们只能靠「计数非零」这个**巧合**，
/// 而一次进程重启就会让计数归零，它们悄悄变回可领取。
#[tokio::test]
async fn 从未认领的身份也能永久退出池子() {
    let api = Api::new().await;
    // 排序上 deploy-test 在 user0001 前面，所以不退役的话它必然先被挑中。
    api.seed_claimable_pool(&["node-a"], &["deploy-test", "user0001"]).await;

    let count = proxy_manager::identity::retire(
        &api.store,
        "deploy-test",
        "kimi",
        "部署侧测试身份，凭据在 verify 脚本里流通",
    )
    .await
    .expect("从未认领的槽位也该退得掉");
    assert_eq!(count, 1);

    let logged_in = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    let claim = logged_in.claim.expect("还有 user0001 可领");
    assert_eq!(claim.identity_name, "user0001", "退役掉的那个不该再被挑中");
}

/// 退役之后池子就空了——登录仍然成功，只是没有身份。
#[tokio::test]
async fn 全部退役之后池子为空() {
    let api = Api::new().await;
    api.seed_claimable_pool(&["node-a"], &["deploy-test"]).await;
    proxy_manager::identity::retire(&api.store, "deploy-test", "kimi", "测试身份").await.unwrap();

    let logged_in = proxy_manager::sso::complete_login(
        &api.store,
        &Roster::empty(),
        &identity("alice", None),
        time::Duration::hours(8),
    )
    .await
    .unwrap();
    assert!(logged_in.claim.is_none(), "池子空了就是没有，不是给一个退役的");
}

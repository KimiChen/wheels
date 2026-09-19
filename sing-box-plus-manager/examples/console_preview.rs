//! 控制台预览服务器：**只用于开发时看页面**，不进发布二进制。
//!
//! 为什么需要它：M4 把 12 页原型接上了真实 API，而「接上了」这件事
//! 只有把页面真的渲染一遍才能验——Rust 侧的断言能证明服务端发了什么，
//! 证明不了浏览器把它画成了什么。绑定引擎漏一个字段、模板里多留一行原型示例数据，
//! 在字符串断言里全是绿的。
//!
//! 登录入口属于 M5（README §11）。在那之前这里**直接签一张会话**，
//! 所以它放在 `examples/` 下：`cargo install` 不会带上它，
//! systemd 单元里也引用不到。
//!
//! ```text
//! cargo run --example console_preview
//! ```
//!
//! 数据全部是 RFC 保留段与占位名——`wheels` 是公开仓库，示例数据里
//! 不出现任何真实节点、域名或账号。

use std::sync::Arc;

use proxy_manager::api::session::{self, Role};
use proxy_manager::config::StorageConfig;
use proxy_manager::store::Store;
use sqlx::Row;
use time::OffsetDateTime;

const PORT: u16 = 8787;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let dir = std::env::temp_dir().join("proxy-manager-preview");
    std::fs::create_dir_all(&dir)?;
    let path = dir.join("preview.db");
    // 每次重来：预览库没有需要保留的东西，重建比对齐状态便宜（D22 同理）。
    let _ = std::fs::remove_file(&path);

    let store = Arc::new(Store::open(&StorageConfig { path, busy_timeout_ms: 5_000 }).await?);
    store.init_schema().await?;
    // 与 `run_service` 同一条纪律：单行设置在这里建出来，`quota::settings::apply`
    // 只负责改。少了它，趋势数据铺完之后下一步就崩。
    proxy_manager::quota::settings::ensure_initialized(&store, "Asia/Shanghai").await?;
    seed(&store).await?;

    // 两个角色各签一张：管理员视角与普通用户视角要能来回切，
    // 否则「越权拦截」这件事只在测试里成立，在眼前看不到。
    let admin = session::create(&store, 1, "local", time::Duration::hours(8)).await?;
    let member = session::create(&store, 2, "local", time::Duration::hours(8)).await?;
    let audit_dir = dir.join("audit");
    let _ = std::fs::remove_dir_all(&audit_dir);
    seed_audit(&audit_dir)?;

    let listener = tokio::net::TcpListener::bind(("127.0.0.1", PORT)).await?;

    println!("\n预览地址  http://127.0.0.1:{PORT}/overview.html");
    for (label, issued) in [("管理员", &admin), ("普通用户", &member)] {
        println!(
            "{label}  document.cookie = '{}={}; path=/; secure'",
            session::SESSION_COOKIE,
            issued.cookie_value
        );
    }
    println!("普通用户带着自己的 cookie 打开 /nodes.html 应当是 403，而不是一张藏起来的管理页\n");

    let (_tx, rx) = tokio::sync::watch::channel(false);
    // 预览用 `local` 会话，所以角色取自 `users.role`，不需要成员名单（D27）。
    // 预览不带订阅：`/sub/…` 不挂载，`me.html` 上那块保持占位。
    // **带审计**：这一页的价值全在「看见自己的出站目标」上，而那件事只有把
    // 真实形状的 JSONL 铺进去、让它走完解析 → C35 裁剪 → 聚合这条链才看得见。
    proxy_manager::api::serve(
        store.clone(),
        std::sync::Arc::new(Vec::new()),
        None,
        None,
        Some(proxy_manager::config::AuditConfig { dir: audit_dir, interval_secs: 600 }),
        listener,
        rx,
    )
    .await?;
    Ok(())
}

/// 造一份**够把每条绑定都点亮**的数据：两个角色、两个节点、一次额度变更。
async fn seed(store: &Store) -> anyhow::Result<()> {
    let now = "2026-09-18T00:00:00Z";
    let cycle = proxy_manager::quota::pool::cycle_key(time::OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;

    for (login, display, role) in
        [("admin", "管理员", Role::Admin), ("member", "普通用户", Role::User)]
    {
        sqlx::query(
            "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
             VALUES (?, ?, ?, 'normal', 'active', ?, ?)",
        )
        .bind(login)
        .bind(display)
        .bind(role.as_str())
        .bind(now)
        .bind(now)
        .execute(txn.conn())
        .await?;
    }

    // 地址用 RFC 5737 的文档保留段。
    for (node_id, address) in [("node-a", "203.0.113.11:8443"), ("node-b", "203.0.113.12:8443")] {
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', ?, 'baseline', ?, 1, 'active', ?, ?)",
        )
        .bind(node_id)
        .bind(address)
        .bind("0".repeat(64))
        .bind(now)
        .bind(now)
        .execute(txn.conn())
        .await?;
    }

    // 周期用量：一个接近上限、一个刚开始。**故意挑一个大到会被浮点削掉的数**——
    // 前端如果哪天退回 Number()，这一页会立刻显示错值，而不是等真实用户撞上。
    for (user_id, total) in [(1_i64, 263_066_238_976_u128), (2, 18_446_744_073_709_551_000)] {
        sqlx::query(
            "INSERT INTO usage_cycle_totals(user_id, cycle_key, rule_version, total_bytes, updated_at) \
             VALUES (?, ?, ?, ?, ?)",
        )
        .bind(user_id)
        .bind(&cycle)
        .bind(proxy_manager::ledger::settle::CYCLE_RULE_VERSION)
        .bind(format!("{total:039}"))
        .bind(now)
        .execute(txn.conn())
        .await?;
    }
    txn.commit().await?;

    seed_trend(store, "node-a").await?;

    // 走真实的 apply：设置、revision、审计与待下发任务必须同事务落库，
    // 预览里也不绕过它，否则看到的收敛状态是假的。
    proxy_manager::quota::settings::apply(
        store,
        &proxy_manager::quota::settings::Scope::Group {
            group_name: "normal".into(),
            new_monthly_bytes: 300 * 1024 * 1024 * 1024,
        },
        "preview",
        &cycle,
        true,
    )
    .await?;

    let count: i64 =
        sqlx::query("SELECT count(*) FROM users").fetch_one(store.readers()).await?.get(0);
    println!("已造数据：{count} 个用户、2 个节点、1 次额度变更");
    Ok(())
}

/// 给趋势图造一段小时级流量。
///
/// 这里**直接写账本行**，而不是像 `tests/m4/trend.rs` 那样走真实结算——
/// 走结算要先跑 `scripts/export-node-fixtures.py` 拿上游样例，给预览服务器
/// 加一个 Python 依赖不划算。在**测试**里这么抄近路是不行的
/// （用例会和被测代码共用同一个键格式化函数，键漂移就永远测不出来），
/// 但预览不是测试：桶键格式的一致性已经由那组用例证明。
async fn seed_trend(store: &Store, node_id: &str) -> anyhow::Result<()> {
    use proxy_manager::ledger::bucket;
    use time::Duration;

    let now = OffsetDateTime::now_utc();
    let stamp = bucket::to_rfc3339(now);
    let mut txn = store.begin_immediate().await?;

    let runtime_pk: i64 = sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         approval_status, approval_reason, approved_by, approved_at, window_state, first_seen_at, last_seen_at) \
         VALUES (?, ?, ?, 'baseline', 'approved', '预览数据', 'preview', ?, 'open', ?, ?) \
         RETURNING runtime_pk",
    )
    .bind(node_id)
    .bind("b1c2d3e4f5061728394a5b6c7d8e9f01")
    .bind(format!("{:020}", 1_700_000_000_000_u64))
    .bind(&stamp)
    .bind(&stamp)
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?
    .get(0);

    let service_id: i64 = sqlx::query(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, \
         active, first_seen_at) VALUES (?, 'ss-in', ?, 'shadowsocks', 1, ?) \
         RETURNING runtime_service_id",
    )
    .bind(runtime_pk)
    .bind(format!("{:020}", 1_u64))
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?
    .get(0);

    // **归属不写在这里。** `runtime_identities` 按 (node, runtime) 键，节点一重启
    // 就是全新的行，写在这里的归属会静默全部丢失——schema 里那一列早就删了，
    // 而这个示例一直还在往它里面写，于是它启动即崩。没人发现是因为没有任何东西
    // 会跑它：Rust 侧的断言证明得了服务端发了什么，证明不了页面画成了什么，
    // 而这个示例正是用来看后者的。
    let identity_id: i64 = sqlx::query(
        "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, identity_name, \
         generation, active, first_seen_at, last_seen_at) \
         VALUES (?, ?, 'u_example_01', ?, 1, ?, ?) RETURNING runtime_identity_id",
    )
    .bind(runtime_pk)
    .bind(service_id)
    .bind(format!("{:020}", 1_u64))
    .bind(&stamp)
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?
    .get(0);

    // 归属落在**槽位**上，跨重启存活。审计裁剪（C35）也是从这里取历史持有区间。
    let route_id: i64 = sqlx::query(
        "INSERT INTO identity_routes(node_id, identity_name, state, user_id, \
         claimed_at, created_at) VALUES (?, 'u_example_01', 'claimed', 2, ?, ?) \
         RETURNING route_id",
    )
    .bind(node_id)
    .bind(&stamp)
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?
    .get(0);
    // 持有起点放到一年前：预览里的审计记录跨度可以到 30 天，
    // 起点设成「刚才」的话那些记录会被正确地判成「持有之前」而全部不显示，
    // 然后人会以为是页面坏了。
    sqlx::query(
        "INSERT INTO identity_assignment_events(route_id, user_id, state, effective_from, \
         recorded_at) VALUES (?, 2, 'assigned', '2026-01-01T00:00:00Z', ?)",
    )
    .bind(route_id)
    .bind(&stamp)
    .execute(txn.conn())
    .await?;

    // 24 小时，**留几个空桶**：空桶要能在图上看见，
    // 否则「这段时间没有流量」会被压成两根相邻的柱子。
    for hour in 0..24_i64 {
        if matches!(hour, 5 | 6 | 7 | 17) {
            continue;
        }
        let at = bucket::hour_bucket(now) - Duration::hours(hour);
        let key = bucket::to_rfc3339(at);
        let base = 40_000_000_u64 + (hour as u64 % 7) * 13_000_000;
        let batch_pk: i64 = sqlx::query(
            "INSERT INTO snapshot_batches(node_id, runtime_id, runtime_pk, sequence, status, \
             payload_sha256, collected_at, received_at, accounting_at, time_quality, created_at) \
             VALUES (?, ?, ?, ?, 'applied', ?, ?, ?, ?, 'trusted', ?) RETURNING batch_pk",
        )
        .bind(node_id)
        .bind("b1c2d3e4f5061728394a5b6c7d8e9f01")
        .bind(runtime_pk)
        .bind(format!("{:020}", (100 - hour) as u64))
        .bind("0".repeat(64))
        .bind(&key)
        .bind(&key)
        .bind(&key)
        .bind(&stamp)
        .fetch_one(txn.conn())
        .await?
        .get(0);

        sqlx::query(
            "INSERT INTO usage_ledger(batch_pk, batch_id, runtime_identity_id, user_id, \
             tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes, \
             source_event_hash, hash_version, accounting_at, hour_bucket, created_at) \
             VALUES (?, ?, ?, 2, ?, ?, ?, ?, ?, 1, ?, ?, ?)",
        )
        .bind(batch_pk)
        .bind(format!("preview-{hour:02}"))
        .bind(identity_id)
        .bind(format!("{base:020}"))
        .bind(format!("{:020}", base * 9))
        .bind(format!("{:020}", base / 20))
        .bind(format!("{:020}", base / 11))
        .bind(format!("{hour:064}"))
        .bind(&key)
        .bind(&key)
        .bind(&stamp)
        .execute(txn.conn())
        .await?;
    }
    txn.commit().await?;
    println!("已造趋势数据：24 小时中 20 个桶有流量，4 个空桶");
    Ok(())
}

/// 铺一份**真实形状**的审计 JSONL。
///
/// 13 个键、键名与类型照 `internal/userstats/audit.go` 的 `encode()`，
/// 三种 `host_src` 各有，外加两条诊断行——`ev=gap` 的 `n` 一条给了数、一条是
/// `null`，因为页面必须在后一种情况下显示「—」而不是编一个数出来。
///
/// 域名用 RFC 2606 的保留段。`wheels` 是公开仓库，示例数据里不出现任何真实
/// 节点、域名或账号。
fn seed_audit(root: &std::path::Path) -> anyhow::Result<()> {
    let node = root.join("node-a");
    std::fs::create_dir_all(&node)?;
    let now = OffsetDateTime::now_utc().unix_timestamp() * 1_000;
    let run = "b1c2d3e4f5061728394a5b6c7d8e9f01";

    let mut lines = Vec::new();
    let mut seq = 0u64;
    // 文件头的排除规则留痕。节点每次打开物理文件都写一行，页面的「有些目标我们不记录」
    // 那一块就是从它取的——所以预览里必须有它，否则那一块永远看不到。
    seq += 1;
    lines.push(format!(
        r#"{{"seq":{seq},"ev":"filter","hosts":["apple.com","github.com","google.com","microsoft.com"],"ips":["198.18.0.0/15","17.253.0.0/16"]}}"#
    ));
    // 同一个域名来一批，让「次数」这一列有东西可排。
    for (host, source, port, hits, ago_ms) in [
        ("www.example.com", "sniff", 443u16, 37u32, 3_600_000i64),
        ("api.example.net", "sniff", 443, 12, 7_200_000),
        ("git.example.org", "fqdn", 22, 4, 86_400_000),
        ("203.0.113.9", "ip", 9000, 2, 172_800_000),
    ] {
        for hit in 0..hits {
            seq += 1;
            lines.push(format!(
                r#"{{"down":{down},"host":"{host}","host_src":"{source}","in":"ss-in","ms":{ms},"net":"tcp","node":"node-a","port":{port},"run":"{run}","seq":{seq},"ts":{ts},"up":{up},"user":"u_example_01"}}"#,
                down = 3_800 + hit as u64 * 7,
                ms = 40 + hit % 30,
                ts = now - ago_ms + hit as i64 * 1_000,
                up = 1_500 + hit as u64 * 3,
            ));
        }
    }
    // 一条 TCP 只上行没下行：**节点会写，主控要自己复核判据**，所以它不该出现在
    // 页面上。留着它，是为了让「复核」这件事在预览里也是可证的。
    seq += 1;
    lines.push(format!(
        r#"{{"down":0,"host":"never-shown.example.com","host_src":"sniff","in":"ss-in","ms":9,"net":"tcp","node":"node-a","port":443,"run":"{run}","seq":{seq},"ts":{ts},"up":800,"user":"u_example_01"}}"#,
        ts = now - 600_000,
    ));
    seq += 1;
    lines.push(format!(
        r#"{{"seq":{seq},"ev":"gap","after":{after},"n":9,"reason":"queue_full"}}"#,
        after = seq - 1
    ));
    seq += 1;
    lines.push(format!(
        r#"{{"seq":{seq},"ev":"gap","after":{after},"n":null,"reason":"total_cap"}}"#,
        after = seq - 1
    ));

    let name = proxy_manager_wire::audit::active_file_name("u_example_01");
    let mut body = lines.join("\n");
    body.push('\n');
    std::fs::write(node.join(name), body)?;

    // 粘滞到 plus 重启的那一位。页面要能把它与 ev=gap 分开显示——
    // 前者范围开口，后者有边界。
    proxy_manager::audit::health::note_dropped(&node, "node-a", run, "2026-09-19T06:00:00Z")?;
    println!("已造审计数据：{} 行，含 2 条缺口与 1 条 audit_dropped", lines.len());
    Ok(())
}

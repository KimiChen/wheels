//! 计量守护：每 poll_interval 拉取各认证身份流量 → 汇总用户增量 → 判配额/有效期 → reconcile 停用/恢复。
//! 增量对内核重启/计数归零稳健（cur<last 时 delta=cur）。用户可选择按月、按年或永不重置。

use crate::app::Shared;
use crate::backend::reload::ReloadBackend;
use crate::backend::ssm::SsmBackend;
use crate::backend::{Backend, Desired};
use crate::config::{Config, ResetCycle};
use crate::db;
use crate::secrets::Secrets;
use anyhow::{bail, Context, Result};
use sqlx::SqlitePool;
use std::collections::{BTreeMap, BTreeSet};
use std::time::Duration;
use time::{Month, OffsetDateTime};
use tokio_util::sync::CancellationToken;

fn build_backend(cfg: &Config) -> Result<Box<dyn Backend>> {
    match cfg.backend.mode.as_str() {
        "ssm" => Ok(Box::new(
            SsmBackend::new(&cfg.backend.ssm_base)?.with_inbounds(vec!["in-shared".to_string()]),
        )),
        "reload" => {
            let grpc = cfg
                .backend
                .stats_grpc
                .clone()
                .unwrap_or_else(|| "http://127.0.0.1:8080".to_string());
            Ok(Box::new(ReloadBackend::new(
                cfg.singbox.config_out.clone(),
                grpc,
                cfg.backend.reload_cmd.clone(),
                cfg.singbox.inbound.kind == "vless-reality",
            )))
        }
        m => bail!("未知 backend.mode {m:?}（ssm | reload）"),
    }
}

/// 当前计费周期键（周期起点所在年月）。reset_day=1 => 日历月。
pub fn current_period(now: OffsetDateTime, reset_day: u8) -> String {
    usage_period(now, reset_day, ResetCycle::Monthly)
}

/// 用户当前计费周期键。月度键保持 `YYYY-MM`，兼容已有数据库；年度为 `YYYY`，永不重置为 `never`。
pub fn usage_period(now: OffsetDateTime, reset_day: u8, reset: ResetCycle) -> String {
    let reset_day = reset_day.clamp(1, 31);
    match reset {
        ResetCycle::Monthly => monthly_period(now, reset_day),
        ResetCycle::Yearly => {
            let mut year = now.year();
            if now.month() == Month::January && now.day() < reset_day {
                year -= 1;
            }
            format!("{year:04}")
        }
        ResetCycle::Never => "never".to_string(),
    }
}

fn monthly_period(now: OffsetDateTime, reset_day: u8) -> String {
    let mut y = now.year();
    let mut mm = u8::from(now.month()) as i32;
    // 29..31 日在短月份按该月最后一天处理，避免出现整月无法进入新周期。
    let effective_reset_day = reset_day.min(now.month().length(now.year()));
    if now.day() < effective_reset_day {
        mm -= 1;
        if mm == 0 {
            mm = 12;
            y -= 1;
        }
    }
    format!("{y:04}-{mm:02}")
}

fn parse_interval(s: &str) -> Result<Duration> {
    let s = s.trim();
    let idx = s.find(|c: char| !c.is_ascii_digit()).unwrap_or(s.len());
    let (num, unit) = s.split_at(idx);
    let n: u64 = num.parse().context("poll_interval 数字")?;
    let secs = match unit.trim() {
        "" | "s" => n,
        "m" => n * 60,
        "h" => n * 3600,
        o => anyhow::bail!("poll_interval 单位 {o:?}（s/m/h）"),
    };
    Ok(Duration::from_secs(secs.max(1)))
}

pub async fn run(shared: Shared, db: SqlitePool, cancel: CancellationToken) -> Result<()> {
    let first = shared.load_full();
    let be = build_backend(&first.cfg)?;
    let interval = parse_interval(&first.cfg.service.poll_interval)?;
    println!(
        "metering 启动：mode={} poll={:?} reset_day={} 用户={} 出口={} 入口端口={}",
        first.cfg.backend.mode,
        interval,
        first.cfg.service.reset_day,
        first.cfg.users.len(),
        first.cfg.all_exits().len(),
        first.cfg.singbox.entry_port,
    );
    drop(first);
    let mut ticker = tokio::time::interval(interval);
    loop {
        tokio::select! {
            _ = cancel.cancelled() => { println!("[meter] stopped"); break; }
            _ = ticker.tick() => {
                let data = shared.load_full();
                if let Err(e) = tick(&data.cfg, &data.sec, &db, be.as_ref()).await {
                    eprintln!("[tick error] {e:#}");
                }
            }
        }
    }
    Ok(())
}

/// 把 config 的用户（配额/有效期）同步进 DB；config 里删掉的用户从 DB 清除。
async fn sync_users(cfg: &Config, db: &SqlitePool) -> Result<()> {
    let now = OffsetDateTime::now_utc();
    for (name, u) in &cfg.users {
        let quota = crate::parse_quota(&u.quota)? as i64;
        let expire = crate::parse_expire(&u.expire)?;
        let period = usage_period(now, cfg.service.reset_day, u.reset);
        db::sync_user(db, name, quota, Some(expire), u.reset.as_str(), &period).await?;
    }
    let in_cfg: BTreeSet<&str> = cfg.users.keys().map(String::as_str).collect();
    for name in db::all_user_names(db).await? {
        if !in_cfg.contains(name.as_str()) {
            db::delete_user(db, &name).await?;
        }
    }
    Ok(())
}

/// 单次轮询周期（暴露给测试/一次性运行）。
pub async fn tick(cfg: &Config, sec: &Secrets, db: &SqlitePool, be: &dyn Backend) -> Result<()> {
    sync_users(cfg, db).await?; // 热重载后的用户/配额/有效期变更在这里落地
    let now = OffsetDateTime::now_utc();

    // 1) 拉每 (认证身份,scope) 流量，以独立基线计算增量，再汇总进所属主用户。
    // 统计源失败仍继续计算停用状态；SSM 可继续动态下发，reload 模式则必须跳过 apply，
    // 避免在计数尚未读取时重载 sing-box 并永久丢失内存统计。
    let stats_ok = match be.read_stats().await {
        Ok(stats) => {
            for s in stats {
                let Some((account, _exit)) = sec.access_owner(&s.name) else {
                    continue;
                };
                let Some(user) = cfg.users.get(account) else {
                    continue;
                };
                let period = usage_period(now, cfg.service.reset_day, user.reset);
                let (lu, ld) = db::get_baseline(db, &s.name, &s.scope).await?;
                let (cu, cd) = (s.up as i64, s.down as i64);
                let dup = if cu >= lu { cu - lu } else { cu };
                let ddown = if cd >= ld { cd - ld } else { cd };
                if dup > 0 || ddown > 0 {
                    db::add_usage(db, account, &period, dup, ddown).await?;
                }
                db::set_baseline(db, account, &s.name, &s.scope, cu, cd).await?;
            }
            true
        }
        Err(e) => {
            eprintln!("[read_stats 失败，本轮跳过计量累加] {e:#}");
            false
        }
    };

    // 2) 判定停用（超额 or 到期），更新 disabled，算出启用集
    let now_ts = now.unix_timestamp();
    let mut enabled: BTreeSet<String> = BTreeSet::new();
    for (name, user) in &cfg.users {
        let period = usage_period(now, cfg.service.reset_day, user.reset);
        let (quota, expire) = db::user_limits(db, name).await?;
        let (u, d) = db::period_usage(db, name, &period).await?;
        let used_bytes = (u + d).max(0) as u64;
        let expired = expire.map(|e| now_ts >= e).unwrap_or(false);
        let over = quota > 0 && used_bytes >= quota as u64;
        let disabled = expired || over;
        let prev = db::get_disabled(db, name).await?;
        db::set_disabled(db, name, disabled).await?;
        if !disabled {
            enabled.insert(name.clone());
        }
        // 只在状态翻转时记一行，避免每 tick 刷屏
        if disabled != prev {
            let why = if expired {
                "到期"
            } else if over {
                "超额"
            } else {
                "恢复"
            };
            let quota_s = if quota == 0 {
                "∞".to_string()
            } else {
                quota.to_string()
            };
            println!(
                "[{name}] {}（{why}）周期 {period} 用量 {used_bytes} / {quota_s}",
                if disabled { "停用" } else { "启用" }
            );
        }
    }
    println!("[tick] 启用 {}/{}", enabled.len(), cfg.users.len());

    // 3) 构建期望身份集并下发（SSM: 增删；reload: 改配置 users[] + 重载）
    let vless = cfg.singbox.inbound.kind == "vless-reality";
    let mut want: BTreeMap<String, String> = BTreeMap::new();
    for (name, user) in &cfg.users {
        if enabled.contains(name) {
            for exit in &user.exits {
                let access = sec.access(name, exit);
                let cred = if vless {
                    access.uuid.clone()
                } else {
                    access.upsk.clone()
                };
                want.insert(access.name.clone(), cred);
            }
        }
    }
    let mut desired: Desired = BTreeMap::new();
    desired.insert("in-shared".to_string(), want);
    if !stats_ok && cfg.backend.mode == "reload" {
        eprintln!("[read_stats 失败，reload 模式本轮跳过身份下发]");
        return Ok(());
    }
    be.apply(&desired).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{current_period, tick, usage_period};
    use crate::backend::{Backend, Desired, UserStat};
    use crate::config::Config;
    use crate::config::ResetCycle;
    use crate::secrets::Secrets;
    use anyhow::Result;
    use async_trait::async_trait;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;
    use time::macros::datetime;

    struct FakeBackend {
        stats: Vec<UserStat>,
        applied: Mutex<Option<Desired>>,
    }

    #[async_trait]
    impl Backend for FakeBackend {
        async fn read_stats(&self) -> Result<Vec<UserStat>> {
            Ok(self.stats.clone())
        }

        async fn apply(&self, desired: &Desired) -> Result<()> {
            *self.applied.lock().unwrap() = Some(desired.clone());
            Ok(())
        }
    }

    struct FailingBackend {
        apply_calls: AtomicUsize,
    }

    #[async_trait]
    impl Backend for FailingBackend {
        async fn read_stats(&self) -> Result<Vec<UserStat>> {
            anyhow::bail!("stats unavailable")
        }

        async fn apply(&self, _desired: &Desired) -> Result<()> {
            self.apply_calls.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
    }

    #[test]
    fn period_cycles() {
        // reset_day=1 => 日历月
        assert_eq!(current_period(datetime!(2027-03-15 0:00 UTC), 1), "2027-03");
        // 未到 reset_day => 归上一周期
        assert_eq!(
            current_period(datetime!(2027-03-15 0:00 UTC), 20),
            "2027-02"
        );
        // 跨年回退
        assert_eq!(
            current_period(datetime!(2027-01-05 0:00 UTC), 10),
            "2026-12"
        );
        // 已过 reset_day
        assert_eq!(
            current_period(datetime!(2027-01-20 0:00 UTC), 10),
            "2027-01"
        );
        // 31 日在二月按月末处理
        assert_eq!(
            current_period(datetime!(2027-02-27 0:00 UTC), 31),
            "2027-01"
        );
        assert_eq!(
            current_period(datetime!(2027-02-28 0:00 UTC), 31),
            "2027-02"
        );
    }

    #[test]
    fn yearly_and_never_cycles() {
        assert_eq!(
            usage_period(datetime!(2027-01-09 0:00 UTC), 10, ResetCycle::Yearly),
            "2026"
        );
        assert_eq!(
            usage_period(datetime!(2027-01-10 0:00 UTC), 10, ResetCycle::Yearly),
            "2027"
        );
        assert_eq!(
            usage_period(datetime!(2099-12-31 23:59 UTC), 10, ResetCycle::Never),
            "never"
        );
    }

    #[tokio::test]
    async fn tick_aggregates_access_identities_into_account_usage() {
        let cfg: Config = toml::from_str(
            r#"
[service]
listen = "127.0.0.1:9736"
public_host = "entry.example.com"
sub_base_url = "https://sub.example.com/sub"
poll_interval = "30s"
reset_day = 1
db_path = "/tmp/sbm.db"

[singbox]
config_out = "/tmp/config.json"
entry_port = 19736
relay_method = "2022-blake3-aes-128-gcm"

[singbox.inbound]
type = "shadowsocks"
method = "2022-blake3-aes-128-gcm"

[backend]
mode = "ssm"
ssm_base = "http://127.0.0.1:8081"

[nodes]
entry = "192.0.2.1"
home = "192.0.2.2"

[exits]
entry = []
home = ["entry"]

[users.alice]
quota = "10G"
expire = "2099-01-01"
exits = ["entry", "home"]
"#,
        )
        .unwrap();
        let sec: Secrets = toml::from_str(
            r#"
server_psk = "entry-key"

[user.alice]
token = "token"

[user.alice.access.entry]
name = "alice-entry"
upsk = "alice-entry-key"
uuid = "00000000-0000-4000-8000-000000000001"

[user.alice.access.home]
name = "alice-home"
upsk = "alice-home-key"
uuid = "00000000-0000-4000-8000-000000000002"
"#,
        )
        .unwrap();
        let backend = FakeBackend {
            stats: vec![
                UserStat {
                    name: "alice-entry".to_string(),
                    scope: "in-shared".to_string(),
                    up: 10,
                    down: 20,
                },
                UserStat {
                    name: "alice-home".to_string(),
                    scope: "in-shared".to_string(),
                    up: 30,
                    down: 40,
                },
            ],
            applied: Mutex::new(None),
        };
        let path = std::env::temp_dir().join(format!("sbm-meter-{}.db", uuid::Uuid::new_v4()));
        let pool = crate::db::open(&path.to_string_lossy()).await.unwrap();

        tick(&cfg, &sec, &pool, &backend).await.unwrap();

        let period = usage_period(
            time::OffsetDateTime::now_utc(),
            cfg.service.reset_day,
            ResetCycle::Monthly,
        );
        assert_eq!(
            crate::db::period_usage(&pool, "alice", &period)
                .await
                .unwrap(),
            (40, 60)
        );
        let applied = backend.applied.lock().unwrap().clone().unwrap();
        let shared = applied.get("in-shared").unwrap();
        assert_eq!(
            shared.get("alice-entry"),
            Some(&"alice-entry-key".to_string())
        );
        assert_eq!(
            shared.get("alice-home"),
            Some(&"alice-home-key".to_string())
        );

        pool.close().await;
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{suffix}", path.display()));
        }
    }

    #[tokio::test]
    async fn reload_skips_apply_when_stats_read_fails() {
        let cfg: Config = toml::from_str(
            r#"
[service]
listen = "127.0.0.1:9736"
public_host = "entry.example.com"
sub_base_url = "https://sub.example.com/sub"
poll_interval = "30s"
reset_day = 1
db_path = "/tmp/sbm.db"

[singbox]
config_out = "/tmp/config.json"
entry_port = 19736
relay_method = "2022-blake3-aes-128-gcm"

[singbox.inbound]
type = "shadowsocks"
method = "2022-blake3-aes-128-gcm"

[backend]
mode = "reload"
ssm_base = "http://127.0.0.1:8081"
stats_grpc = "http://127.0.0.1:8082"
reload_cmd = "true"

[nodes]
entry = "192.0.2.1"

[exits]
entry = []

[users.alice]
quota = "10G"
expire = "2099-01-01"
exits = ["entry"]
"#,
        )
        .unwrap();
        let sec: Secrets = toml::from_str(
            r#"
server_psk = "entry-key"

[user.alice]
token = "token"

[user.alice.access.entry]
name = "alice-entry"
upsk = "alice-entry-key"
uuid = "00000000-0000-4000-8000-000000000001"
"#,
        )
        .unwrap();
        let backend = FailingBackend {
            apply_calls: AtomicUsize::new(0),
        };
        let path = std::env::temp_dir().join(format!("sbm-meter-{}.db", uuid::Uuid::new_v4()));
        let pool = crate::db::open(&path.to_string_lossy()).await.unwrap();

        tick(&cfg, &sec, &pool, &backend).await.unwrap();

        assert_eq!(backend.apply_calls.load(Ordering::SeqCst), 0);

        pool.close().await;
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{suffix}", path.display()));
        }
    }
}

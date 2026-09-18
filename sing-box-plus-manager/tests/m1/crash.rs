//! 事务中途 `kill -9` 后的自洽（README §8）。
//!
//! §8 的原文是「账本/累计/游标事务中途 **kill -9**」，不是「事务返回错误」。
//! 两者证明的东西不同：错误返回证明的是「错误路径会回滚」，
//! 而 SIGKILL 证明的是「**进程不执行任何清理代码**时，SQLite 的原子提交仍然成立」。
//! 断电与 OOM 是后者的形态，所以这里真的起一个子进程再把它杀掉。
//!
//! 用 `Child::kill()`（Unix 上就是 SIGKILL）而不是让子进程自己 `abort()`：
//! `abort()` 是 SIGABRT，进程仍会跑 atexit 与部分清理。

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use proxy_manager::config::StorageConfig;
use proxy_manager::store::Store;
use sqlx::Row;

use super::ledger_harness::{Snapshot, NODE, RUNTIME, STARTED_AT_MS};

const BIN: &str = env!("CARGO_BIN_EXE_proxy-manager");

struct Scene {
    dir: tempfile::TempDir,
    config_dir: PathBuf,
    db: PathBuf,
    snapshot: PathBuf,
}

fn scene(snapshot: &Snapshot) -> Scene {
    let dir = tempfile::tempdir().expect("临时目录");
    let config_dir = dir.path().join("etc");
    std::fs::create_dir_all(&config_dir).unwrap();
    let db = dir.path().join("pm.db");

    std::fs::write(
        config_dir.join("server.toml"),
        format!(
            r#"
[listen]
api = "127.0.0.1:8173"
[storage]
path = "{}"
[collect]
interval_secs = 60
min_interval_secs = 10
snapshot_max_age_secs = 120
[quota]
monthly_bytes = "322122547200"
display_timezone = "Asia/Shanghai"
"#,
            db.display()
        ),
    )
    .unwrap();
    std::fs::write(
        config_dir.join("nodes.toml"),
        format!(
            r#"
[[node]]
node_id = "{NODE}"
address = "node-01.example.com:8443"
first_snapshot = "baseline"
snapshot_socket = "/run/sing-box-plus/user_stats.sock"
quota_socket = "/run/sing-box-plus/user_stats_quota.sock"
agent_spki_sha256 = "{}"
materials_dir = "/etc/proxy-manager/materials/node"
"#,
            "0".repeat(64)
        ),
    )
    .unwrap();

    let snapshot_path = dir.path().join("snapshot.json");
    std::fs::write(&snapshot_path, snapshot.bytes()).unwrap();

    Scene { dir, config_dir, db, snapshot: snapshot_path }
}

impl Scene {
    fn approve(&self) {
        let status = Command::new(BIN)
            .args(["runtime", "approve"])
            .arg("--config-dir")
            .arg(&self.config_dir)
            .args(["--node", NODE, "--runtime", RUNTIME])
            .arg("--started-at-ms")
            .arg(STARTED_AT_MS.to_string())
            .args(["--policy", "include", "--reason", "用例", "--actor", "test"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .expect("运行 runtime approve");
        assert!(status.success(), "批准 runtime 失败");
    }

    /// 正常跑一次 ingest。
    fn ingest(&self) -> std::process::Output {
        Command::new(BIN)
            .args(["ledger", "ingest"])
            .arg("--config-dir")
            .arg(&self.config_dir)
            .args(["--node", NODE])
            .arg(&self.snapshot)
            .output()
            .expect("运行 ledger ingest")
    }

    /// 在指定崩溃点把子进程**真的 SIGKILL 掉**。
    fn ingest_and_kill(&self, point: &str) {
        let marker = self.dir.path().join(format!("marker-{point}"));
        let mut child = Command::new(BIN)
            .args(["ledger", "ingest"])
            .arg("--config-dir")
            .arg(&self.config_dir)
            .args(["--node", NODE])
            .arg(&self.snapshot)
            .env("PM_CRASH_AT", point)
            .env("PM_CRASH_MARKER", &marker)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("拉起子进程");

        // 等它真的到达崩溃点。到不了就说明崩溃点没编进去或没被执行——
        // 那种情况必须判失败，不能当成「跑过了」。
        let deadline = Instant::now() + Duration::from_secs(30);
        while !marker.exists() {
            if let Ok(Some(status)) = child.try_wait() {
                panic!("子进程在到达崩溃点 {point} 之前就退出了（{status}）：崩溃点没生效");
            }
            assert!(Instant::now() < deadline, "等待崩溃点 {point} 超时");
            std::thread::sleep(Duration::from_millis(20));
        }

        // SIGKILL。进程不会执行任何清理代码。
        child.kill().expect("发送 SIGKILL");
        let status = child.wait().expect("回收子进程");
        assert!(!status.success(), "被 SIGKILL 的进程不该是成功退出");
    }

    async fn open(&self) -> Store {
        let config = StorageConfig { path: self.db.clone(), busy_timeout_ms: 5_000 };
        Store::open(&config).await.expect("重新打开数据库")
    }
}

async fn scalar(store: &Store, sql: &str) -> i64 {
    sqlx::query(sql).fetch_one(store.readers()).await.unwrap().get(0)
}

async fn lifetime_sum(store: &Store) -> u128 {
    let rows = sqlx::query(
        "SELECT tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes \
         FROM usage_lifetime_totals",
    )
    .fetch_all(store.readers())
    .await
    .unwrap();
    rows.iter()
        .flat_map(|row| {
            (0..4).map(move |index| {
                row.get::<String, _>(index).trim_start_matches('0').parse::<u128>().unwrap_or(0)
            })
        })
        .sum()
}

/// **最值钱的一条**：全部写完、就差 COMMIT 时被杀，整笔必须回滚。
#[tokio::test]
async fn 提交前被kill整笔回滚且可重试() {
    let scene = scene(&Snapshot::new());
    scene.approve();
    scene.ingest_and_kill("before_commit");

    {
        let store = scene.open().await;
        // 账本、累计、游标——四张表必须一起回滚。
        assert_eq!(scalar(&store, "SELECT count(*) FROM usage_ledger").await, 0, "账本必须回滚");
        assert_eq!(
            scalar(&store, "SELECT count(*) FROM usage_lifetime_totals").await,
            0,
            "持久累计必须回滚"
        );
        assert_eq!(scalar(&store, "SELECT count(*) FROM counter_cursors").await, 0, "游标必须回滚");
        // 批次保持 pending：**自然可重试**，这正是「不持久化中间 processing 状态」的效果。
        assert_eq!(
            scalar(&store, "SELECT count(*) FROM snapshot_batches WHERE status = 'pending'").await,
            1,
            "批次必须保持 pending"
        );
        // 原始字节在接收阶段就已经落盘了，崩溃不该带走它——
        // 它是结算输入，没有它就无法重试。
        assert_eq!(scalar(&store, "SELECT count(*) FROM snapshot_payloads").await, 1);
        store.close().await;
    }

    // 重试必须成功，且**不重复**。
    let output = scene.ingest();
    assert!(output.status.success(), "重试应当成功：{}", String::from_utf8_lossy(&output.stderr));

    let store = scene.open().await;
    assert_eq!(scalar(&store, "SELECT count(*) FROM usage_ledger").await, 2, "样例里两个非零身份");
    assert_eq!(lifetime_sum(&store).await, 312, "300 + 12，一分不多一分不少");
    assert_eq!(
        scalar(&store, "SELECT count(*) FROM snapshot_batches WHERE status = 'applied'").await,
        1
    );
    store.close().await;
}

/// COMMIT 已返回之后被杀：一切必须是持久的，而且重跑不重复。
#[tokio::test]
async fn 提交后被kill数据持久且重跑不重复() {
    let scene = scene(&Snapshot::new());
    scene.approve();
    scene.ingest_and_kill("after_commit");

    let expected = {
        let store = scene.open().await;
        assert_eq!(scalar(&store, "SELECT count(*) FROM usage_ledger").await, 2, "提交后必须持久");
        assert_eq!(lifetime_sum(&store).await, 312);
        assert_eq!(
            scalar(&store, "SELECT count(*) FROM snapshot_batches WHERE status = 'applied'").await,
            1
        );
        let total = lifetime_sum(&store).await;
        store.close().await;
        total
    };

    // 同一份快照再喂一次：同号同摘要幂等，不得二次入账。
    let output = scene.ingest();
    assert!(output.status.success());
    let store = scene.open().await;
    assert_eq!(scalar(&store, "SELECT count(*) FROM usage_ledger").await, 2, "不得重复入账");
    assert_eq!(lifetime_sum(&store).await, expected, "累计不得被加第二次");
    store.close().await;
}

/// 事务中间的三个点各杀一次，四张表必须**一起**回滚。
///
/// 只测「就差 COMMIT」那个点是不够的：如果实现把某一步拆出了单独的事务，
/// 那一步之后被杀就会留下半截数据，而那种实现在「就差 COMMIT」这条下仍然是绿的。
#[tokio::test]
async fn 事务中间任一点被kill都不留半截数据() {
    for point in ["after_ledger_rows", "after_totals", "after_cursors"] {
        let scene = scene(&Snapshot::new());
        scene.approve();
        scene.ingest_and_kill(point);

        let store = scene.open().await;
        for (table, label) in [
            ("usage_ledger", "账本"),
            ("usage_lifetime_totals", "持久累计"),
            ("counter_cursors", "游标"),
        ] {
            let count = scalar(&store, &format!("SELECT count(*) FROM {table}")).await;
            assert_eq!(count, 0, "在 {point} 被杀之后 {label} 必须回滚，实际有 {count} 行");
        }
        let applied =
            scalar(&store, "SELECT count(*) FROM snapshot_batches WHERE status = 'applied'").await;
        assert_eq!(applied, 0, "在 {point} 被杀之后不该有 applied 批次");
        store.close().await;
    }
}

/// 反向证据：崩溃点没被触发时，同一条路径必须正常走完。
///
/// 没有这一条，上面三条对一个「永远写不进去」的实现同样是绿的。
#[tokio::test]
async fn 不注入崩溃时正常入账() {
    let scene = scene(&Snapshot::new());
    scene.approve();
    let output = scene.ingest();
    assert!(output.status.success(), "{}", String::from_utf8_lossy(&output.stderr));

    let store = scene.open().await;
    assert_eq!(scalar(&store, "SELECT count(*) FROM usage_ledger").await, 2);
    assert_eq!(lifetime_sum(&store).await, 312);
    store.close().await;
}

// 崩溃点必须被编进测试构建——否则上面几条会退化成「什么都没验」。
// 做成**编译期**错误而不是运行时断言：一个「因为构建方式不对所以没真跑」的测试，
// 应该连编译都过不去，而不是绿着跑完。
#[cfg(not(any(debug_assertions, feature = "crash-points")))]
compile_error!(
    "崩溃用例必须跑在带崩溃点的构建上（debug，或显式开 crash-points）——skip 不等于 pass"
);

/// 被测二进制确实存在。
#[test]
fn 被测二进制存在() {
    assert!(Path::new(BIN).exists(), "找不到被测二进制：{BIN}");
}

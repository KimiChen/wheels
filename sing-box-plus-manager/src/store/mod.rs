//! SQLite 存储层：单写者队列 + 短 `BEGIN IMMEDIATE` 事务（D8、`docs/data-model.md` §3.1）。
//!
//! SQLite 没有 `SELECT ... FOR UPDATE` 行锁。本项目不模拟行锁，改为
//! **单写者队列**：全进程只有一条写连接，读改写在同一连接内完成。
//! WAL 让并发读不阻塞写，但**不会增加写入者数量**——这条常被误解成「WAL = 多写者」。
//!
//! 事务内不调用网络、不等待节点、不跑长聚合。这不是性能建议：
//! 写事务持有期间整库只有它能写，一次网络超时就会把结算队列整个卡住。

pub mod codec;
pub mod schema;
pub mod verify;

use std::path::PathBuf;

use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous};
use sqlx::{Connection, SqliteConnection, SqlitePool};
use tokio::sync::{Mutex, MutexGuard};

use crate::config::StorageConfig;
use crate::error::Result;

pub use codec::{U128Text, U64Text, U128_TEXT_WIDTH, U64_TEXT_WIDTH};
pub use schema::InitOutcome;

/// 写连接及其事务状态。
///
/// `txn_open` 是为了让**没有显式结束的事务**有确定的收场：`Drop` 里不能 `await`，
/// 所以不在 Drop 里回滚，而是把标记留着，由下一次 `begin_immediate` 先 `ROLLBACK` 清场。
/// 这样「持有 guard 时 panic 了」不会让后续写入静默地跑在一个别人开的事务里。
struct Writer {
    conn: SqliteConnection,
    txn_open: bool,
}

pub struct Store {
    readers: SqlitePool,
    writer: Mutex<Writer>,
    path: PathBuf,
}

impl Store {
    pub async fn open(config: &StorageConfig) -> Result<Self> {
        if let Some(parent) = config.path.parent() {
            if !parent.as_os_str().is_empty() {
                let _ = std::fs::create_dir_all(parent);
            }
        }
        let options = SqliteConnectOptions::new()
            .filename(&config.path)
            .create_if_missing(true)
            .journal_mode(SqliteJournalMode::Wal)
            .foreign_keys(true)
            .synchronous(SqliteSynchronous::Normal)
            .busy_timeout(std::time::Duration::from_millis(config.busy_timeout_ms as u64));

        // 读池可以有多条连接；写只有下面那一条。
        let readers =
            SqlitePoolOptions::new().max_connections(4).connect_with(options.clone()).await?;
        let conn = SqliteConnection::connect_with(&options).await?;

        Ok(Store {
            readers,
            writer: Mutex::new(Writer { conn, txn_open: false }),
            path: config.path.clone(),
        })
    }

    /// 只读地打开一份**已经存在**的库，用于核对备份与恢复出来的副本。
    ///
    /// 三处与 [`Store::open`] 刻意不同，每一处都是为了**不改动被检查的那份文件**：
    ///
    /// * `create_if_missing(false)`——路径打错时要报错，而不是凭空造一个空库
    ///   然后报告「表都不在」。那个结论看起来像「备份坏了」，其实是路径写错了。
    /// * 不设 `journal_mode`——`VACUUM INTO` 出来的备份是 delete 模式，
    ///   而把它切成 WAL 会**写回文件头**：一次「只读检查」改掉了被检查的对象。
    /// * `read_only(true)`——任何写入直接失败，而不是悄悄成功。
    pub async fn open_readonly(path: &std::path::Path) -> Result<Self> {
        if !path.exists() {
            return Err(crate::error::Error::invalid_config(
                "§5",
                format!("{} 不存在", path.display()),
            ));
        }
        let options = SqliteConnectOptions::new()
            .filename(path)
            .create_if_missing(false)
            .read_only(true)
            .foreign_keys(true)
            .busy_timeout(std::time::Duration::from_millis(5_000));
        let readers =
            SqlitePoolOptions::new().max_connections(2).connect_with(options.clone()).await?;
        let conn = SqliteConnection::connect_with(&options).await?;
        Ok(Store {
            readers,
            writer: Mutex::new(Writer { conn, txn_open: false }),
            path: path.to_path_buf(),
        })
    }

    /// 为**备份**打开一份已有的库：不建库、不建表、不同步节点。
    ///
    /// `open_store()` 那条路径会依次做 `create_if_missing(true)` → `init_schema()`
    /// → `sync_nodes()`——**后两件都是写**。一个备份工具在动手之前先改一遍
    /// 被备份的对象，是把「备份」和「一次可能失败的写入」绑在了一起；
    /// 而 `create_if_missing` 让一个打错的路径变成一个崭新的空库，
    /// 然后它会被认认真真地备份、核对、并报告「一切正常」。
    ///
    /// 先试只读。WAL 的读连接需要写 `-shm`，所以服务停着又留着 `-wal` 时只读会失败——
    /// 那时退回读写打开（SQLite 需要它来跑 WAL 恢复），但**仍然不建表、不同步**。
    ///
    /// **不加 `immutable=1`。** 搜「read-only database file」最容易搜到的就是它，
    /// 而它会让 SQLite **完全忽略 WAL**：打开成功，读到的是 checkpoint 之前的旧数据，
    /// 而且 `integrity_check` 照样报 ok。
    pub async fn open_for_backup(path: &std::path::Path) -> Result<Self> {
        if !path.exists() {
            return Err(crate::error::Error::invalid_config(
                "§5",
                format!("{} 不存在：备份不会凭空建一个空库出来", path.display()),
            ));
        }
        let base = SqliteConnectOptions::new()
            .filename(path)
            .create_if_missing(false)
            .foreign_keys(true)
            .busy_timeout(std::time::Duration::from_millis(5_000));
        for read_only in [true, false] {
            let options = base.clone().read_only(read_only);
            let Ok(readers) =
                SqlitePoolOptions::new().max_connections(2).connect_with(options.clone()).await
            else {
                continue;
            };
            let Ok(conn) = SqliteConnection::connect_with(&options).await else { continue };
            if !read_only {
                tracing::warn!(
                    "只读打开失败（多半是服务停着且留着 -wal），改用读写打开做 WAL 恢复"
                );
            }
            return Ok(Store {
                readers,
                writer: Mutex::new(Writer { conn, txn_open: false }),
                path: path.to_path_buf(),
            });
        }
        Err(crate::error::Error::invalid_config("§5", format!("打不开 {}", path.display())))
    }

    /// 备份前对**源库**的两条前置断言。
    ///
    /// 一、`journal_mode` 必须是 `wal`。这是一条便宜的机械 tell：
    /// 在 URI 上加 `immutable=1` 之后，SQLite 会**完全跳过 WAL**——
    /// 打开成功、`integrity_check` 报 ok、读到的却是 checkpoint 之前的旧数据，
    /// 而它把 WAL 库自报成 `delete`。搜「unable to open database file」
    /// 最容易搜到的解法就是加这个参数，而它会把备份静默降级成「只拷了主文件」。
    /// 直接 grep 参数名抓不住等价写法（打开标志也能开 immutable），自报模式能。
    ///
    /// 二、SQLite 版本要够 `VACUUM INTO`（3.27）。今天链接的是 sqlx 带进来的
    /// bundled 3.46，但**这件事今天就已经是一条没人检查的假设**——
    /// 有人改掉 feature 就会换成系统库，而那时缺的是一条断言，不是一条注释。
    pub async fn assert_backup_preconditions(&self) -> Result<()> {
        use sqlx::Row as _;
        let mode: String =
            sqlx::query("PRAGMA journal_mode").fetch_one(&self.readers).await?.get(0);
        if !mode.eq_ignore_ascii_case("wal") {
            return Err(crate::error::Error::invalid_config(
                "§10 R3",
                format!(
                    "源库自报 journal_mode = {mode}，而主控一律用 WAL。                     最常见的成因是连接串里带了 immutable=1——那会让 SQLite 跳过 WAL，                     备出来的是 checkpoint 之前的旧数据，而且 integrity_check 照样报 ok"
                ),
            ));
        }
        let version: String =
            sqlx::query("SELECT sqlite_version()").fetch_one(&self.readers).await?.get(0);
        let parts: Vec<u32> = version.split('.').map(|p| p.parse().unwrap_or(0)).collect();
        let too_old = match parts.as_slice() {
            [major, minor, ..] => *major < 3 || (*major == 3 && *minor < 27),
            _ => true,
        };
        if too_old {
            return Err(crate::error::Error::invalid_config(
                "§10 R3",
                format!("SQLite {version} 没有 VACUUM INTO（要 3.27 以上）"),
            ));
        }
        Ok(())
    }

    pub fn path(&self) -> &std::path::Path {
        &self.path
    }

    /// 并发读用的连接池（WAL）。
    pub fn readers(&self) -> &SqlitePool {
        &self.readers
    }

    /// 建库或确认已建好。**不是迁移**：本项目不做迁移功能（D22）。
    pub async fn init_schema(&self) -> Result<InitOutcome> {
        let mut writer = self.writer.lock().await;
        self.clear_stale_txn(&mut writer).await?;
        schema::initialize(&mut writer.conn).await
    }

    /// 取得写事务。调用点必须显式 `commit()` 或 `rollback()`。
    ///
    /// 返回的 guard 持有单写者互斥量，因此**在它活着的期间不要做任何等待外部的事**。
    pub async fn begin_immediate(&self) -> Result<WriteTxn<'_>> {
        let mut writer = self.writer.lock().await;
        self.clear_stale_txn(&mut writer).await?;
        sqlx::query("BEGIN IMMEDIATE").execute(&mut writer.conn).await?;
        writer.txn_open = true;
        Ok(WriteTxn { writer, finished: false })
    }

    /// 上一次事务没有显式结束（多半是 guard 被 drop 或调用点 panic 了）时先清场。
    async fn clear_stale_txn(&self, writer: &mut MutexGuard<'_, Writer>) -> Result<()> {
        if writer.txn_open {
            tracing::warn!("上一次写事务没有显式结束，先回滚清场");
            let _ = sqlx::query("ROLLBACK").execute(&mut writer.conn).await;
            writer.txn_open = false;
        }
        Ok(())
    }

    pub async fn close(self) {
        self.readers.close().await;
        let writer = self.writer.into_inner();
        let _ = writer.conn.close().await;
    }
}

/// 一个开着的 `BEGIN IMMEDIATE` 写事务。
///
/// 刻意不提供 `transaction(|conn| ...)` 那种闭包封装：结算事务的 11 步需要在中途
/// 按读到的状态决定是整份拒绝还是继续（`docs/data-model.md` §4），
/// 闭包封装会诱导把「拒绝」写成错误返回，而拒绝和失败在这里不是一回事——
/// 可审计的拒绝还要留一条批次回执。
pub struct WriteTxn<'a> {
    writer: MutexGuard<'a, Writer>,
    finished: bool,
}

impl WriteTxn<'_> {
    pub fn conn(&mut self) -> &mut SqliteConnection {
        &mut self.writer.conn
    }

    pub async fn commit(mut self) -> Result<()> {
        sqlx::query("COMMIT").execute(&mut self.writer.conn).await?;
        self.writer.txn_open = false;
        self.finished = true;
        Ok(())
    }

    pub async fn rollback(mut self) -> Result<()> {
        sqlx::query("ROLLBACK").execute(&mut self.writer.conn).await?;
        self.writer.txn_open = false;
        self.finished = true;
        Ok(())
    }
}

impl Drop for WriteTxn<'_> {
    fn drop(&mut self) {
        if !self.finished {
            // 这里不能 await。`txn_open` 仍为 true，下一次 begin_immediate 会先 ROLLBACK。
            tracing::warn!("写事务未显式结束就被丢弃：将在下一次取事务时回滚");
        }
    }
}

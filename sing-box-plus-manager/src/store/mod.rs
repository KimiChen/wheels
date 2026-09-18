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

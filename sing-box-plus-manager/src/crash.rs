//! 崩溃注入点。**release 构建里不存在**。
//!
//! README §8 的故障矩阵里有一条：「账本/累计/游标事务中途 `kill -9`」。
//! 要真的验它，就必须在一个**真实进程**的事务中途被外部信号杀死，
//! 而不是在进程内模拟一个错误返回——后者证明的是「错误路径会回滚」，
//! 不是「进程被 SIGKILL 时 SQLite 的原子提交仍然成立」。
//!
//! 为什么是「挂起等外部 SIGKILL」而不是自己 `abort()`：
//! `abort()` 触发的是 SIGABRT，进程仍会跑 atexit 与部分清理；
//! 而 SIGKILL 不给进程任何执行机会，这才是断电/OOM 的形态。
//!
//! 编译门禁是 `debug_assertions` 或显式的 `crash-points` 特性。
//! 为什么不是「只看特性」：那样 `cargo test` 默认就跑不到崩溃用例，
//! 而「环境不支持所以跳过」正是 README §8 明令禁止的——**skip 不等于 pass**。
//! 挂在 `debug_assertions` 上，默认的 `cargo test` 一定会真的杀一次进程；
//! 而 `--release` 出的生产二进制里这几行编译后不留任何分支——
//! 一个「只在测试里才存在」的钩子不该出现在装到机器上的东西里。
//! 需要在 staging 的 release 二进制上演练时，再显式开 `crash-points`。

/// 到达一个命名的崩溃点。
///
/// 只有同时满足两个条件才会挂起：编译时开了 `crash-points` 特性，
/// 且运行时 `PM_CRASH_AT` 等于这个点的名字。
#[cfg(any(feature = "crash-points", debug_assertions))]
pub fn checkpoint(point: &str) {
    let Ok(target) = std::env::var("PM_CRASH_AT") else { return };
    if target != point {
        return;
    }
    // 先落一个标记文件，父进程据此知道「已经到位，可以杀了」。
    // 不用管道或信号：这条路径要在事务持锁期间可靠工作，越笨越好。
    if let Ok(marker) = std::env::var("PM_CRASH_MARKER") {
        let _ = std::fs::write(&marker, point);
    }
    eprintln!("[crash-points] 已到达 {point}，挂起等待外部 SIGKILL");
    loop {
        std::thread::sleep(std::time::Duration::from_millis(20));
    }
}

#[cfg(not(any(feature = "crash-points", debug_assertions)))]
#[inline(always)]
pub fn checkpoint(_point: &str) {}

/// 崩溃点的名字。集中在这里，避免测试与实现各写一份字符串。
pub mod points {
    /// 账本行已插入，持久累计还没更新。
    pub const AFTER_LEDGER_ROWS: &str = "after_ledger_rows";
    /// 持久累计已更新，游标还没推进。
    pub const AFTER_TOTALS: &str = "after_totals";
    /// 游标已推进，批次状态还没改。
    pub const AFTER_CURSORS: &str = "after_cursors";
    /// 全部写完，**就差 COMMIT**。这是最值钱的一个点。
    pub const BEFORE_COMMIT: &str = "before_commit";
    /// COMMIT 已返回。此时一切都必须是持久的。
    pub const AFTER_COMMIT: &str = "after_commit";
}

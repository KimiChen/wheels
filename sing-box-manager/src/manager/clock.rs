//! 可注入时钟。生产用 [`SystemClock`]；调度器/轮询的超时重试逻辑用 [`TestClock`] 做确定性测试。

use std::sync::atomic::{AtomicI64, Ordering};

pub trait Clock: Send + Sync {
    fn now_unix(&self) -> i64;
}

pub struct SystemClock;

impl Clock for SystemClock {
    fn now_unix(&self) -> i64 {
        crate::store::now_unix()
    }
}

/// 手动推进的测试时钟。
pub struct TestClock {
    t: AtomicI64,
}

impl TestClock {
    pub fn new(start: i64) -> Self {
        Self {
            t: AtomicI64::new(start),
        }
    }
    pub fn advance(&self, secs: i64) {
        self.t.fetch_add(secs, Ordering::SeqCst);
    }
}

impl Clock for TestClock {
    fn now_unix(&self) -> i64 {
        self.t.load(Ordering::SeqCst)
    }
}

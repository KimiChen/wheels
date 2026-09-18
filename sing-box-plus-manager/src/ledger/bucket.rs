//! 三个时间与归桶（`docs/data-model.md` §6）。
//!
//! 每份快照保存三个时间，**并持久化归桶选择结果**——否则重放同一批次会落进另一个桶。
//!
//! | 字段 | 来源 | 用途 |
//! | --- | --- | --- |
//! | `collected_at` | agent 完成采集的时刻 | 时间质量判定 |
//! | `received_at` | 主控收到响应的可信时刻 | 偏差核对；`collected_at` 不可信时的回落值 |
//! | `accounting_at` | 主控按规则生成 | 增量归属的时间桶 |
//!
//! 口径三元组随事实一起固定，其中 `time_bucket_estimated = true` **不是笔误**：
//! 快照只有累计值、没有逐次传输的事件时间，所以两次成功快照之间的增量被整体归到
//! 本次采集所在的桶。同一健康 runtime 内累计差分保持字节守恒，
//! 但**时间不确定范围是两次成功采集的实际间隔**，可能远大于调度周期（R11）。

use time::OffsetDateTime;

/// 节点必须启用 NTP（C27）。主控把**节点时钟偏移**作为一等健康项采集并告警。
///
/// 这不是洁癖：Shadowsocks 2022 的时间戳防重放让一台慢约两分钟的节点**所有**握手失败，
/// 表现为「整节点不可用」而非「某用户不可用」，不查时钟会一路误判。
pub const DEFAULT_MAX_CLOCK_SKEW_SECS: i64 = 120;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TimeQuality {
    /// `collected_at` 可信，`accounting_at` 用它。
    Trusted,
    /// `collected_at` 不可信，回落到 `received_at`。**回落本身要落库并告警。**
    Fallback,
}

impl TimeQuality {
    pub fn as_str(self) -> &'static str {
        match self {
            TimeQuality::Trusted => "trusted",
            TimeQuality::Fallback => "fallback",
        }
    }
}

/// 为什么回落。告警要说得出原因，否则运维只知道「时间质量差」。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FallbackReason {
    /// `collected_at` 比 `received_at` 还新超过阈值——节点时钟快了。
    TooFarInFuture,
    /// `collected_at` 比 `received_at` 旧超过阈值——节点时钟慢了，或链路上卡了很久。
    TooFarInPast,
    /// 同一 runtime 内 `collected_at` 倒退了。
    WentBackwards,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimeDecision {
    pub collected_at: OffsetDateTime,
    pub received_at: OffsetDateTime,
    pub accounting_at: OffsetDateTime,
    pub quality: TimeQuality,
    pub fallback_reason: Option<FallbackReason>,
    /// 两次**成功采集**的实际间隔。首次为 `None`。
    ///
    /// 这个数就是时间不确定范围。停采多轮之后它会远大于调度周期——
    /// 那时不能按一轮估算，必须如实披露（R11）。
    pub observation_gap_ms: Option<u64>,
    /// 节点时钟偏移（秒，带符号）。正数表示节点比主控快。作为一等健康项告警（C27）。
    pub clock_skew_secs: i64,
}

/// 按 §6 的规则决定归桶时间。
///
/// `previous_collected_at` 是**同一 runtime** 上一次成功采集的 `collected_at`；
/// 跨 runtime 不比较——runtime 换了，累计值清零，时间也不连续。
pub fn decide(
    collected_at: OffsetDateTime,
    received_at: OffsetDateTime,
    previous_collected_at: Option<OffsetDateTime>,
    max_skew_secs: i64,
) -> TimeDecision {
    let clock_skew_secs = (collected_at - received_at).whole_seconds();

    let fallback_reason = if clock_skew_secs > max_skew_secs {
        Some(FallbackReason::TooFarInFuture)
    } else if clock_skew_secs < -max_skew_secs {
        Some(FallbackReason::TooFarInPast)
    } else if previous_collected_at.is_some_and(|previous| collected_at < previous) {
        // 同一 runtime 内单调不倒退。倒退说明节点时钟被调过，
        // 用它归桶会让后一批增量落进比前一批更早的桶里。
        Some(FallbackReason::WentBackwards)
    } else {
        None
    };

    let (accounting_at, quality) = match fallback_reason {
        Some(_) => (received_at, TimeQuality::Fallback),
        None => (collected_at, TimeQuality::Trusted),
    };

    // 间隔按**归桶实际用的那个时间**算，而不是固定用 collected_at：
    // 回落之后 collected_at 已经被判定为不可信，拿它算间隔等于把不可信的数
    // 换个字段再报一次。
    let observation_gap_ms = previous_collected_at.and_then(|previous| {
        let millis = (accounting_at - previous).whole_milliseconds();
        u64::try_from(millis).ok()
    });

    TimeDecision {
        collected_at,
        received_at,
        accounting_at,
        quality,
        fallback_reason,
        observation_gap_ms,
        clock_skew_secs,
    }
}

/// 小时桶：UTC 整点 + **半开区间 `[start, end)`**。返回区间的 start。
pub fn hour_bucket(at: OffsetDateTime) -> OffsetDateTime {
    at.replace_minute(0)
        .expect("0 是合法的分钟")
        .replace_second(0)
        .expect("0 是合法的秒")
        .replace_nanosecond(0)
        .expect("0 是合法的纳秒")
        .to_offset(time::UtcOffset::UTC)
}

/// RFC 3339（UTC）。时间在应用层统一编码为 UTC，SQLite 不依赖连接会话时区。
pub fn to_rfc3339(at: OffsetDateTime) -> String {
    at.to_offset(time::UtcOffset::UTC)
        .format(&time::format_description::well_known::Rfc3339)
        .expect("OffsetDateTime 总能格式化成 RFC 3339")
}

pub fn parse_rfc3339(text: &str) -> Option<OffsetDateTime> {
    OffsetDateTime::parse(text, &time::format_description::well_known::Rfc3339).ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use time::macros::datetime;

    const SKEW: i64 = DEFAULT_MAX_CLOCK_SKEW_SECS;

    #[test]
    fn 正常情况用collected_at归桶() {
        let collected = datetime!(2026-09-18 10:30:15 UTC);
        let received = datetime!(2026-09-18 10:30:16 UTC);
        let decision = decide(collected, received, None, SKEW);
        assert_eq!(decision.quality, TimeQuality::Trusted);
        assert_eq!(decision.accounting_at, collected);
        assert_eq!(decision.fallback_reason, None);
        assert_eq!(decision.clock_skew_secs, -1);
    }

    #[test]
    fn 节点时钟快超阈值时回落() {
        let received = datetime!(2026-09-18 10:00:00 UTC);
        let collected = received + time::Duration::seconds(SKEW + 1);
        let decision = decide(collected, received, None, SKEW);
        assert_eq!(decision.quality, TimeQuality::Fallback);
        assert_eq!(decision.fallback_reason, Some(FallbackReason::TooFarInFuture));
        assert_eq!(decision.accounting_at, received);
        assert_eq!(decision.clock_skew_secs, SKEW + 1);
    }

    #[test]
    fn 节点时钟慢超阈值时回落() {
        let received = datetime!(2026-09-18 10:00:00 UTC);
        let collected = received - time::Duration::seconds(SKEW + 1);
        let decision = decide(collected, received, None, SKEW);
        assert_eq!(decision.fallback_reason, Some(FallbackReason::TooFarInPast));
        assert_eq!(decision.accounting_at, received);
    }

    /// 边界上仍然可信——否则阈值就名不副实了。
    #[test]
    fn 恰好在阈值上不回落() {
        let received = datetime!(2026-09-18 10:00:00 UTC);
        for offset in [SKEW, -SKEW] {
            let collected = received + time::Duration::seconds(offset);
            assert_eq!(decide(collected, received, None, SKEW).quality, TimeQuality::Trusted);
        }
    }

    #[test]
    fn 同一runtime内倒退时回落() {
        let previous = datetime!(2026-09-18 10:00:00 UTC);
        let collected = datetime!(2026-09-18 09:59:59 UTC);
        let received = datetime!(2026-09-18 10:00:01 UTC);
        let decision = decide(collected, received, Some(previous), SKEW);
        assert_eq!(decision.fallback_reason, Some(FallbackReason::WentBackwards));
        assert_eq!(decision.accounting_at, received);
    }

    /// 反向证据：不倒退就不该回落。否则「凡是有上一次就回落」也能让上面那条变绿。
    #[test]
    fn 不倒退时不回落() {
        let previous = datetime!(2026-09-18 10:00:00 UTC);
        let collected = datetime!(2026-09-18 10:01:00 UTC);
        let received = datetime!(2026-09-18 10:01:01 UTC);
        let decision = decide(collected, received, Some(previous), SKEW);
        assert_eq!(decision.quality, TimeQuality::Trusted);
        assert_eq!(decision.observation_gap_ms, Some(60_000));
    }

    /// 停采多轮之后间隔远大于调度周期，必须如实报出来（R11）。
    #[test]
    fn 停采之后的间隔按实际算() {
        let previous = datetime!(2026-09-18 10:00:00 UTC);
        let collected = datetime!(2026-09-18 12:00:00 UTC); // 停了两小时
        let received = collected + time::Duration::seconds(1);
        let decision = decide(collected, received, Some(previous), SKEW);
        assert_eq!(decision.observation_gap_ms, Some(2 * 60 * 60 * 1000));
    }

    #[test]
    fn 首次采集没有间隔() {
        let collected = datetime!(2026-09-18 10:00:00 UTC);
        assert_eq!(decide(collected, collected, None, SKEW).observation_gap_ms, None);
    }

    #[test]
    fn 小时桶是utc整点半开区间() {
        assert_eq!(
            hour_bucket(datetime!(2026-09-18 10:59:59.999 UTC)),
            datetime!(2026-09-18 10:00:00 UTC)
        );
        // 恰好整点属于**本**桶，不属于上一个——半开区间 [start, end)。
        assert_eq!(
            hour_bucket(datetime!(2026-09-18 11:00:00 UTC)),
            datetime!(2026-09-18 11:00:00 UTC)
        );
        // 非 UTC 输入先归一到 UTC 再截断。
        assert_eq!(
            hour_bucket(datetime!(2026-09-18 19:30:00 +08:00)),
            datetime!(2026-09-18 11:00:00 UTC)
        );
    }

    #[test]
    fn rfc3339往返() {
        let at = datetime!(2026-09-18 10:30:15 UTC);
        let text = to_rfc3339(at);
        assert!(text.ends_with('Z'), "统一编码为 UTC：{text}");
        assert_eq!(parse_rfc3339(&text), Some(at));
        assert_eq!(parse_rfc3339("不是时间"), None);
    }
}

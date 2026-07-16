//! 计费周期键（逐字移植 _legacy/meter.rs，含原单测）。月度 `YYYY-MM`（reset_day 边界 + 短月 clamp +
//! 跨年回退）、年度 `YYYY`、永不 `never`。reset_day 全局（settings，默认 1）；reset_cycle 每用户。

use time::{Month, OffsetDateTime};

use crate::domain::user::ResetCycle;

/// unix 秒 → 周期键。
pub fn period_for(now_unix: i64, reset_day: u8, reset: ResetCycle) -> String {
    let now = OffsetDateTime::from_unix_timestamp(now_unix).unwrap_or(OffsetDateTime::UNIX_EPOCH);
    usage_period(now, reset_day, reset)
}

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
    // 29..31 日在短月份按该月最后一天处理，避免整月无法进入新周期。
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

#[cfg(test)]
mod tests {
    use super::*;
    use time::macros::datetime;

    fn current(dt: OffsetDateTime, reset_day: u8) -> String {
        usage_period(dt, reset_day, ResetCycle::Monthly)
    }

    #[test]
    fn period_cycles() {
        assert_eq!(current(datetime!(2027-03-15 0:00 UTC), 1), "2027-03");
        assert_eq!(current(datetime!(2027-03-15 0:00 UTC), 20), "2027-02");
        assert_eq!(current(datetime!(2027-01-05 0:00 UTC), 10), "2026-12");
        assert_eq!(current(datetime!(2027-01-20 0:00 UTC), 10), "2027-01");
        // 短月 clamp：2 月 reset_day=31 → 有效重置日=28。
        assert_eq!(current(datetime!(2027-02-27 0:00 UTC), 31), "2027-01");
        assert_eq!(current(datetime!(2027-02-28 0:00 UTC), 31), "2027-02");
    }

    #[test]
    fn yearly_and_never_cycles() {
        assert_eq!(
            usage_period(datetime!(2027-06-01 0:00 UTC), 1, ResetCycle::Yearly),
            "2027"
        );
        assert_eq!(
            usage_period(datetime!(2027-01-05 0:00 UTC), 10, ResetCycle::Yearly),
            "2026"
        );
        assert_eq!(
            usage_period(datetime!(2027-01-20 0:00 UTC), 10, ResetCycle::Yearly),
            "2027"
        );
        assert_eq!(
            usage_period(datetime!(2027-06-01 0:00 UTC), 1, ResetCycle::Never),
            "never"
        );
    }
}

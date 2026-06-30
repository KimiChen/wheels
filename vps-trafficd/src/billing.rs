use anyhow::{bail, Result};
use chrono::{DateTime, Datelike, FixedOffset, LocalResult, TimeZone, Timelike};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CycleWindow {
    pub start: DateTime<FixedOffset>,
    pub end: DateTime<FixedOffset>,
}

pub fn current_cycle(
    anchor: DateTime<FixedOffset>,
    cycle_months: u32,
    now: DateTime<FixedOffset>,
) -> Result<CycleWindow> {
    if cycle_months == 0 {
        bail!("cycle_months must be greater than zero");
    }

    let now = now.with_timezone(anchor.offset());
    let cycle_months = cycle_months as i32;
    let month_delta = month_index(now) - month_index(anchor);
    let mut period = month_delta.div_euclid(cycle_months);
    let mut start = cycle_boundary(anchor, period, cycle_months)?;

    while start > now {
        period -= 1;
        start = cycle_boundary(anchor, period, cycle_months)?;
    }

    loop {
        let next = cycle_boundary(anchor, period + 1, cycle_months)?;
        if next > now {
            break;
        }
        period += 1;
        start = next;
    }

    Ok(CycleWindow {
        start,
        end: cycle_boundary(anchor, period + 1, cycle_months)?,
    })
}

fn cycle_boundary(
    anchor: DateTime<FixedOffset>,
    period: i32,
    cycle_months: i32,
) -> Result<DateTime<FixedOffset>> {
    add_months(anchor, period * cycle_months)
}

fn month_index(dt: DateTime<FixedOffset>) -> i32 {
    dt.year() * 12 + dt.month0() as i32
}

pub fn add_months(dt: DateTime<FixedOffset>, months: i32) -> Result<DateTime<FixedOffset>> {
    let original_month_index = dt.year() * 12 + dt.month0() as i32;
    let target_month_index = original_month_index + months;
    let year = target_month_index.div_euclid(12);
    let month0 = target_month_index.rem_euclid(12);
    let month = (month0 + 1) as u32;
    let day = dt.day().min(days_in_month(year, month));

    match dt
        .offset()
        .with_ymd_and_hms(year, month, day, dt.hour(), dt.minute(), dt.second())
    {
        LocalResult::Single(value) => Ok(value.with_nanosecond(dt.nanosecond()).unwrap_or(value)),
        _ => bail!("failed to construct billing cycle timestamp"),
    }
}

pub fn days_in_month(year: i32, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 if is_leap_year(year) => 29,
        2 => 28,
        _ => 30,
    }
}

fn is_leap_year(year: i32) -> bool {
    (year % 4 == 0 && year % 100 != 0) || year % 400 == 0
}

//! 趋势查询（README §6、§4.11）。
//!
//! **聚合在 Rust 里做，不用 SQL 的 `SUM` / `CAST`**（C3）：
//! SQLite 的 `SUM` 在 TEXT 上会先转成 REAL，u64 的高位就没了——
//! 而且它不报错，只是把 `18446744073709551615` 悄悄变成 `1.8446744073709552e19`。
//! 字节列存的是定宽零填充的十进制文本，加法一律走 `checked_add`。
//!
//! **小时桶直接用账本里已经持久化的 `hour_bucket`**，不在查询时重算。
//! 重算等于给同一批数据留两套归桶规则，重放时会落进另一个桶（§6）。

use axum::extract::{Query, State};
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::Row;
use time::{Duration, OffsetDateTime};

use super::error::{ApiCode, ApiError, ApiResult};
use super::session::Subject;
use super::AppState;
use crate::store::codec::U128Text;

/// 小时粒度的范围上限。
///
/// 31 天 × 24 = 744 个点，已经超过任何图表能有意义显示的密度；
/// 再往上只是让一次请求扫更多账本行。**上限在 API 层强制**，
/// 不指望前端的下拉框只有三个选项——下拉框不是边界。
const MAX_HOURS: i64 = 31 * 24;

#[derive(Deserialize)]
pub struct TrendQuery {
    /// `24h` / `7d` / `30d`，或直接给小时数。
    range: Option<String>,
    /// `hour`（本轮实现）或 `day`（需要 IANA 时区库，见下）。
    grain: Option<String>,
    /// 只看某个节点；缺省是全部节点。
    node_id: Option<String>,
}

fn hours_of(range: &str) -> Option<i64> {
    match range {
        "24h" => Some(24),
        "7d" => Some(7 * 24),
        "30d" => Some(30 * 24),
        other => other.strip_suffix('h').and_then(|n| n.parse().ok()),
    }
}

/// 四向合计的一个时间桶。
#[derive(Default, Clone, Copy)]
struct Bucket {
    tcp_up: U128Text,
    tcp_down: U128Text,
    udp_up: U128Text,
    udp_down: U128Text,
}

impl Bucket {
    fn add_row(&mut self, row: &sqlx::sqlite::SqliteRow) -> ApiResult<()> {
        // 账本存的是 u64（20 位），累加到 u128（39 位）再回传。
        // **四向分开累加**：合并成一个总量之后就再也拆不回来了（C17）。
        for (slot, index) in [
            (&mut self.tcp_up, 1),
            (&mut self.tcp_down, 2),
            (&mut self.udp_up, 3),
            (&mut self.udp_down, 4),
        ] {
            let text: String = row.get(index);
            let value: u64 = text.trim_start_matches('0').parse().unwrap_or(0);
            *slot = slot.checked_add(U128Text::new(value as u128)).map_err(ApiError::from)?;
        }
        Ok(())
    }

    fn total(&self) -> ApiResult<U128Text> {
        let mut sum = U128Text::new(0);
        for part in [self.tcp_up, self.tcp_down, self.udp_up, self.udp_down] {
            sum = sum.checked_add(part).map_err(ApiError::from)?;
        }
        Ok(sum)
    }
}

pub async fn usage_trend(
    State(state): State<AppState>,
    subject: Subject,
    Query(query): Query<TrendQuery>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;

    let grain = query.grain.as_deref().unwrap_or("hour");
    if grain == "day" {
        // 日桶要按**持久化展示时区的日历日**算，不能写成「起点 + 24 小时」
        // （夏令时与跨月会错，§6）。做对需要一份 IANA 时区库，
        // 那是一条会改变依赖面的决定，不在本轮里悄悄做掉。
        return Err(ApiError::new(ApiCode::AuditNotEnabled, "日粒度尚未启用").with_detail(json!({
            "reason": "日桶须按持久化展示时区的日历日计算，需引入 IANA 时区库",
            "workaround": "小时粒度已覆盖 24h / 7d / 30d 三个范围"
        })));
    }
    if grain != "hour" {
        return Err(ApiError::new(ApiCode::InvalidRequest, "grain 只支持 hour / day")
            .with_detail(json!({ "grain": grain })));
    }

    let range = query.range.as_deref().unwrap_or("24h");
    let Some(hours) = hours_of(range) else {
        return Err(ApiError::new(ApiCode::InvalidRequest, "range 只支持 24h / 7d / 30d")
            .with_detail(json!({ "range": range })));
    };
    if hours <= 0 || hours > MAX_HOURS {
        return Err(ApiError::new(ApiCode::InvalidRequest, "小时粒度范围上限为 31 天")
            .with_detail(json!({ "requested_hours": hours, "max_hours": MAX_HOURS })));
    }

    let now = OffsetDateTime::now_utc();
    // 半开区间 [start, end)：end 是**当前小时的整点**，当前这个未走完的小时不进图，
    // 否则最后一根柱子永远在长，看起来像流量在掉。
    let end = crate::ledger::bucket::hour_bucket(now);
    let start = end - Duration::hours(hours);
    let (start_key, end_key) = (hour_key(start), hour_key(end));

    let rows = match query.node_id.as_deref() {
        Some(node_id) => sqlx::query(
            "SELECT l.hour_bucket, l.tcp_uplink_bytes, l.tcp_downlink_bytes, \
                        l.udp_uplink_bytes, l.udp_downlink_bytes \
                 FROM usage_ledger l \
                 JOIN runtime_identities ri ON ri.runtime_identity_id = l.runtime_identity_id \
                 JOIN node_runtimes nr ON nr.runtime_pk = ri.runtime_pk \
                 WHERE l.hour_bucket >= ? AND l.hour_bucket < ? AND nr.node_id = ?",
        )
        .bind(&start_key)
        .bind(&end_key)
        .bind(node_id),
        None => sqlx::query(
            "SELECT hour_bucket, tcp_uplink_bytes, tcp_downlink_bytes, \
                    udp_uplink_bytes, udp_downlink_bytes \
             FROM usage_ledger WHERE hour_bucket >= ? AND hour_bucket < ?",
        )
        .bind(&start_key)
        .bind(&end_key),
    }
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    // 先把**每一个**桶建出来再填数，空桶要显式出现在结果里。
    // 只回传有数据的桶会让图表把 12 点和 15 点画成相邻两根，
    // 中间那段「没有流量」被压没了——那正是最该看见的东西。
    let mut buckets: Vec<(String, Bucket)> =
        (0..hours).map(|i| (hour_key(start + Duration::hours(i)), Bucket::default())).collect();
    let index: std::collections::HashMap<&str, usize> =
        buckets.iter().enumerate().map(|(i, (key, _))| (key.as_str(), i)).collect();

    let mut placements = Vec::with_capacity(rows.len());
    for row in &rows {
        let key: String = row.get(0);
        placements.push(index.get(key.as_str()).copied());
    }
    for (row, slot) in rows.iter().zip(placements) {
        if let Some(slot) = slot {
            buckets[slot].1.add_row(row)?;
        }
    }

    let mut points = Vec::with_capacity(buckets.len());
    for (key, bucket) in &buckets {
        points.push(json!({
            "bucket": key,
            "tcp_uplink_bytes": bucket.tcp_up.to_api_string(),
            "tcp_downlink_bytes": bucket.tcp_down.to_api_string(),
            "udp_uplink_bytes": bucket.udp_up.to_api_string(),
            "udp_downlink_bytes": bucket.udp_down.to_api_string(),
            "total_bytes": bucket.total()?.to_api_string(),
        }));
    }

    Ok(Json(json!({
        "grain": "hour",
        "range": range,
        "start": start_key,
        "end": end_key,
        "node_id": query.node_id,
        // 口径三元组随事实一起回传（§6）：图上的数字是什么口径，不靠口头解释。
        "metric_scope": crate::api::routes::METRIC_SCOPE,
        "traffic_estimated": false,
        "time_bucket_estimated": true,
        "points": points,
        "next_cursor": Value::Null,
    })))
}

/// 桶键**复用结算侧同一对函数**，不在这里另写一份格式化。
///
/// 两份格式化一定会漂移，而漂移的表现是：查询一个桶都匹配不上，
/// 图表画出一条全零的线——看起来像「这段时间没有流量」，
/// 而真相是「键的写法对不上」。这是本轮写第一版时真的踩到的。
fn hour_key(at: OffsetDateTime) -> String {
    crate::ledger::bucket::to_rfc3339(crate::ledger::bucket::hour_bucket(at))
}

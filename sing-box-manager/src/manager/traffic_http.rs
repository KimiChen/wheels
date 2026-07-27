//! Phase 5：流量/配额只读 Web API。汇总当前周期用量、配额状态与每 Entry 计量健康。
//! 纯读脱敏——不返回任何 uPSK/serverPSK/token 明文，仅字节数与状态枚举。

use axum::extract::State;
use axum::routing::get;
use axum::{Json, Router};
use serde_json::json;

use crate::domain::user::ResetCycle;
use crate::error::AppError;
use crate::manager::http::AppState;
use crate::manager::metering::period::period_for;
use crate::store::{metering, runtime_state, users};

type ApiResult = std::result::Result<Json<serde_json::Value>, AppError>;

pub fn add_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/api/traffic/users", get(traffic_users))
        .route("/api/traffic/entries", get(traffic_entries))
}

/// 每用户当前周期用量 + 配额状态。
async fn traffic_users(State(st): State<AppState>) -> ApiResult {
    let now = crate::store::now_unix();
    let rd = metering::reset_day(&st.pool).await?;
    let mut out = Vec::new();
    for u in users::list_users(&st.pool).await? {
        let cycle = ResetCycle::parse(&u.reset_cycle).unwrap_or(ResetCycle::Monthly);
        let period = period_for(now, rd, cycle);
        let (up, down) = metering::period_usage(&st.pool, &u.id, &period).await?;
        let rs = runtime_state::get_user_state(&st.pool, &u.id).await?;
        out.push(json!({
            "id": u.id,
            "name": u.name,
            "period": period,
            "uplink_bytes": up,
            "downlink_bytes": down,
            "used_bytes": up + down,
            "quota_bytes": u.quota_bytes,
            "reset_cycle": u.reset_cycle,
            "expire_at": u.expire_at,
            "disabled": u.disabled,
            "quota_state": rs.as_ref().map(|s| s.quota_state.clone()),
            "effective_disabled": rs.as_ref().map(|s| s.effective_disabled),
            "over_since": rs.as_ref().and_then(|s| s.over_since),
        }));
    }
    Ok(Json(json!({ "period_reset_day": rd, "users": out })))
}

/// 每 Entry 计量健康（最近成功/失败/连续失败/过期），供运维观测故障隔离。
async fn traffic_entries(State(st): State<AppState>) -> ApiResult {
    let states = runtime_state::list_entry_states(&st.pool).await?;
    Ok(Json(json!({ "entries": states })))
}

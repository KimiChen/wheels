//! 端点实现（README §6）。
//!
//! 每个管理面 handler 的第一件事都是 [`Subject::require_admin`]；
//! 每个个人端点的第一件事都是「目标取服务端会话主体」。
//! **这两件事不能靠前端隐藏导航代劳。**

use axum::extract::{Path, Query, State};
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::Row;
use time::OffsetDateTime;

use crate::api::error::{ApiCode, ApiError, ApiResult, Page};
use crate::api::session::Subject;
use crate::api::{AppState, WriteSubject};
use crate::quota::settings;
use crate::store::codec::{U128Text, U64Text};

/// 字节量一律**十进制字符串**（C3）。
///
/// 这个函数存在的意义是让「哪里返回了裸数字」在 review 时看得出来：
/// 凡是字节量都该经过它。
/// R1：计费口径 ≠ 网卡口径。这句话要随事实一起回传，**只留一份措辞**——
/// 两处各写一遍，改的时候一定只改一处。
pub const METRIC_SCOPE: &str = "认证解码后进入转发边界的应用负载，不含协议 header、隧道封装与重传";

fn bytes_str(value: u128) -> String {
    value.to_string()
}

// ============ 健康 ============

pub async fn healthz() -> impl IntoResponse {
    Json(json!({ "status": "ok" }))
}

/// `readyz` 要真的碰一下库——只回 200 的就绪探针等于没有探针（C22 的同一条）。
pub async fn readyz(State(state): State<AppState>) -> ApiResult<impl IntoResponse> {
    sqlx::query("SELECT 1")
        .fetch_one(state.store.readers())
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    Ok(Json(json!({ "status": "ready" })))
}

// ============ 个人中心 ============

/// 目标**固定取服务端会话主体**，不接受客户端提供的用户 ID。
pub async fn me(State(state): State<AppState>, subject: Subject) -> ApiResult<impl IntoResponse> {
    let cycle = crate::quota::pool::cycle_key(OffsetDateTime::now_utc());
    let used = cycle_used(&state, subject.user_id, &cycle).await?;
    // 额度按用户自己的档位取（定案第三条）。查不到就是 0，不回退到某个全局值——
    // 回退会让一个档位缺失的账号看起来有额度。
    let (group, monthly) = settings::user_quota(&state.store, subject.user_id)
        .await?
        .unwrap_or_else(|| (String::new(), 0));

    Ok(Json(json!({
        "user_id": subject.user_id,
        "login_name": subject.login_name,
        "role": subject.role.as_str(),
        "provider": subject.provider,
        "quota_group": group,
        "cycle": {
            "key": cycle,
            // 三个口径不混用：这里是**周期累计**，不是永久累计。
            "used_bytes": bytes_str(used),
            "monthly_bytes": bytes_str(monthly as u128),
            "remaining_bytes": bytes_str(crate::quota::pool::user_pool(monthly, U128Text::new(used)) as u128),
        },
        // R1：计费口径 ≠ 网卡口径。UI 与订阅页各写一次，不靠口头解释。
        "metric_scope": METRIC_SCOPE,
    })))
}

/// 只统计自己；**节点只显示代号**，不展示地址、端口与转发链细节。
pub async fn me_usage(
    State(state): State<AppState>,
    subject: Subject,
) -> ApiResult<impl IntoResponse> {
    let rows = sqlx::query(
        "SELECT n.node_id, \
                sum(CAST(l.tcp_uplink_bytes AS INTEGER)), sum(CAST(l.tcp_downlink_bytes AS INTEGER)), \
                sum(CAST(l.udp_uplink_bytes AS INTEGER)), sum(CAST(l.udp_downlink_bytes AS INTEGER)) \
         FROM usage_ledger l \
         JOIN runtime_identities i ON i.runtime_identity_id = l.runtime_identity_id \
         JOIN node_runtimes r ON r.runtime_pk = i.runtime_pk \
         JOIN nodes n ON n.node_id = r.node_id \
         WHERE l.user_id = ? GROUP BY n.node_id ORDER BY n.node_id",
    )
    .bind(subject.user_id)
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let nodes: Vec<Value> = rows
        .iter()
        .map(|row| {
            let total: i64 = (1..5).map(|i| row.get::<i64, _>(i)).sum();
            json!({
                // 只给代号。地址、端口与转发链细节不进个人端点。
                "node_id": row.get::<String, _>(0),
                "total_bytes": bytes_str(total as u128),
            })
        })
        .collect();
    Ok(Json(json!({ "nodes": nodes })))
}

// ============ 管理面 ============

#[derive(Deserialize)]
pub struct PageQuery {
    cursor: Option<String>,
    limit: Option<i64>,
}

pub async fn list_nodes(
    State(state): State<AppState>,
    subject: Subject,
    Query(page): Query<PageQuery>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let page = Page::parse(page.cursor.as_deref(), page.limit)?;
    let _ = page.after; // node_id 是文本主键，这一版按名字全量返回

    let rows = sqlx::query(
        "SELECT node_id, provider, address, first_snapshot, quota_enabled, status FROM nodes \
         ORDER BY node_id LIMIT ?",
    )
    .bind(page.limit)
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let mut nodes = Vec::new();
    for row in &rows {
        let node_id: String = row.get(0);
        nodes.push(json!({
            "node_id": node_id,
            "provider": row.get::<String, _>(1),
            "address": row.get::<String, _>(2),
            "first_snapshot": row.get::<String, _>(3),
            "quota_enabled": row.get::<i64, _>(4) == 1,
            "status": row.get::<String, _>(5),
            // C12：缺额度信息时是否放行。取自配置而不是库——签字的事实来源只有一处。
            "fail_open": fail_open_view(&state, &node_id),
            "runtimes": node_runtimes(&state, &node_id).await?,
        }));
    }
    Ok(Json(json!({ "nodes": nodes, "next_cursor": Value::Null })))
}

/// 节点的失败开放状态。`null` 表示失败关闭；否则带上运维签下的理由。
fn fail_open_view(state: &AppState, node_id: &str) -> Value {
    match state.node(node_id).and_then(|node| node.fail_open_reason()) {
        Some(reason) => json!({ "acknowledged_reason": reason }),
        None => Value::Null,
    }
}

pub async fn get_node(
    State(state): State<AppState>,
    subject: Subject,
    Path(node_id): Path<String>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let row = sqlx::query(
        "SELECT node_id, provider, address, first_snapshot, quota_enabled, status FROM nodes \
         WHERE node_id = ?",
    )
    .bind(&node_id)
    .fetch_optional(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::not_found("该节点"))?;

    Ok(Json(json!({
        "node_id": row.get::<String, _>(0),
        "provider": row.get::<String, _>(1),
        "address": row.get::<String, _>(2),
        "first_snapshot": row.get::<String, _>(3),
        "quota_enabled": row.get::<i64, _>(4) == 1,
        "status": row.get::<String, _>(5),
        "fail_open": fail_open_view(&state, &node_id),
        "runtimes": node_runtimes(&state, &node_id).await?,
    })))
}

async fn node_runtimes(state: &AppState, node_id: &str) -> ApiResult<Vec<Value>> {
    let rows = sqlx::query(
        "SELECT runtime_id, started_at_unix_ms, first_snapshot, approval_status, \
                last_sequence, window_state FROM node_runtimes WHERE node_id = ? \
         ORDER BY started_at_unix_ms DESC",
    )
    .bind(node_id)
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;
    rows.into_iter()
        .map(|row| {
            let last_sequence = row
                .get::<Option<String>, _>(4)
                .map(|text| U64Text::decode(&text).map(|v| v.to_api_string()))
                .transpose()
                .map_err(ApiError::from)?;
            Ok(json!({
                "runtime_id": row.get::<String, _>(0),
                // u64 走十进制字符串（C3）。
                "started_at_unix_ms": U64Text::decode(&row.get::<String, _>(1))
                    .map_err(ApiError::from)?.to_api_string(),
                "first_snapshot": row.get::<String, _>(2),
                "approval_status": row.get::<String, _>(3),
                "last_sequence": last_sequence,
                "window_state": row.get::<String, _>(5),
            }))
        })
        .collect()
}

pub async fn list_identities(
    State(state): State<AppState>,
    subject: Subject,
    Query(page): Query<PageQuery>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let page = Page::parse(page.cursor.as_deref(), page.limit)?;
    let rows = sqlx::query(
        // **归属要 join 到 identity_routes，不能从 runtime_identities 上取。**
        // 这里原来写的是 `i.user_id`，而那一列在 schema/02_runtime_lifecycle.sql
        // 里被显式删掉了（注释：「这里曾经有一个 user_id。它是错的地方——本表按
        // (node_id, runtime_id) 键，节点一重启归属就全丢」）。`sqlx::query` 是运行时
        // 检查，所以它编译得过、跑起来 500。抄 `src/quota/table.rs` 的现成写法。
        "SELECT i.runtime_identity_id, r.node_id, r.runtime_id, s.inbound_tag, s.generation, \
                i.identity_name, i.generation, i.active, rt.user_id \
         FROM runtime_identities i \
         JOIN runtime_services s ON s.runtime_service_id = i.runtime_service_id \
         JOIN node_runtimes r ON r.runtime_pk = i.runtime_pk \
         LEFT JOIN identity_routes rt \
                ON rt.node_id = r.node_id AND rt.identity_name = i.identity_name \
         WHERE i.runtime_identity_id > ? ORDER BY i.runtime_identity_id LIMIT ?",
    )
    .bind(page.after.unwrap_or(0))
    .bind(page.limit)
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let next_cursor = (rows.len() as i64 == page.limit)
        .then(|| rows.last().map(|row| row.get::<i64, _>(0).to_string()))
        .flatten();
    let identities: ApiResult<Vec<Value>> = rows
        .iter()
        .map(|row| {
            Ok(json!({
                "id": row.get::<i64, _>(0),
                // 六元组展开：generation 属于协议键，不能因为恒为 1 就省略。
                "node_id": row.get::<String, _>(1),
                "runtime_id": row.get::<String, _>(2),
                "inbound_tag": row.get::<String, _>(3),
                "inbound_generation": U64Text::decode(&row.get::<String, _>(4))
                    .map_err(ApiError::from)?.to_api_string(),
                "identity_name": row.get::<String, _>(5),
                "identity_generation": U64Text::decode(&row.get::<String, _>(6))
                    .map_err(ApiError::from)?.to_api_string(),
                // active=false 标灰但保留（C9）——前端据此渲染，不是过滤掉。
                "active": row.get::<i64, _>(7) == 1,
                "user_id": row.get::<Option<i64>, _>(8),
            }))
        })
        .collect();
    Ok(Json(json!({ "identities": identities?, "next_cursor": next_cursor })))
}

pub async fn list_users(
    State(state): State<AppState>,
    subject: Subject,
    Query(page): Query<PageQuery>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let page = Page::parse(page.cursor.as_deref(), page.limit)?;
    let cycle = crate::quota::pool::cycle_key(OffsetDateTime::now_utc());

    let rows = sqlx::query(
        "SELECT u.user_id, u.login_name, u.display_name, u.role, u.status, \
                u.quota_group, g.monthly_bytes \
         FROM users u JOIN quota_groups g ON g.group_name = u.quota_group \
         WHERE u.user_id > ? ORDER BY u.user_id LIMIT ?",
    )
    .bind(page.after.unwrap_or(0))
    .bind(page.limit)
    .fetch_all(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let next_cursor = (rows.len() as i64 == page.limit)
        .then(|| rows.last().map(|row| row.get::<i64, _>(0).to_string()))
        .flatten();
    let mut users = Vec::new();
    for row in &rows {
        let user_id: i64 = row.get(0);
        let used = cycle_used(&state, user_id, &cycle).await?;
        // 每个人按**自己档位**的额度算剩余。用一个共同的额度去算所有人的剩余，
        // 在分档之后是错的，而且错得不显眼——数字看着都合理。
        let quota = U64Text::decode(&row.get::<String, _>(6))
            .map_err(|e| ApiError::internal(e.to_string()))?
            .get();
        users.push(json!({
            "user_id": user_id,
            "login_name": row.get::<String, _>(1),
            "display_name": row.get::<String, _>(2),
            "role": row.get::<String, _>(3),
            "status": row.get::<String, _>(4),
            "quota_group": row.get::<String, _>(5),
            "monthly_bytes": bytes_str(quota as u128),
            "cycle_used_bytes": bytes_str(used),
            "cycle_remaining_bytes": bytes_str(
                crate::quota::pool::user_pool(quota, U128Text::new(used)) as u128
            ),
        }));
    }
    // 用户身上仍然没有额度字段：这里的 monthly_bytes 来自他所在档位（quota_groups）。
    Ok(Json(json!({ "users": users, "next_cursor": next_cursor })))
}

pub async fn user_usage(
    State(state): State<AppState>,
    subject: Subject,
    Path(user_id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let cycle = crate::quota::pool::cycle_key(OffsetDateTime::now_utc());
    let used = cycle_used(&state, user_id, &cycle).await?;
    let lifetime = sqlx::query(
        "SELECT coalesce(sum(CAST(tcp_uplink_bytes AS INTEGER)), 0) \
              + coalesce(sum(CAST(tcp_downlink_bytes AS INTEGER)), 0) \
              + coalesce(sum(CAST(udp_uplink_bytes AS INTEGER)), 0) \
              + coalesce(sum(CAST(udp_downlink_bytes AS INTEGER)), 0) \
         FROM usage_lifetime_totals WHERE user_id = ?",
    )
    .bind(user_id)
    .fetch_one(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .get::<i64, _>(0);

    Ok(Json(json!({
        "user_id": user_id,
        "cycle": { "key": cycle, "used_bytes": bytes_str(used) },
        // 永久累计**不用来比较周期配额**——两个口径不混用。
        "lifetime_bytes": bytes_str(lifetime as u128),
    })))
}

async fn cycle_used(state: &AppState, user_id: i64, cycle_key: &str) -> ApiResult<u128> {
    let row = sqlx::query(
        "SELECT total_bytes FROM usage_cycle_totals \
         WHERE user_id = ? AND cycle_key = ? AND rule_version = ?",
    )
    .bind(user_id)
    .bind(cycle_key)
    .bind(crate::ledger::settle::CYCLE_RULE_VERSION)
    .fetch_optional(state.store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;
    Ok(match row {
        Some(row) => U128Text::decode(&row.get::<String, _>(0)).map_err(ApiError::from)?.get(),
        None => 0,
    })
}

// ============ 全局额度 ============

pub async fn get_quota_settings(
    State(state): State<AppState>,
    subject: Subject,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let current = settings::load(&state.store).await?;
    let tasks = settings::latest_tasks(&state.store).await?;
    let fully = settings::fully_applied(&state.store).await?;
    let groups = settings::load_groups(&state.store).await?;

    Ok(Json(json!({
        // 四档额度（定案第三条）。额度挂在组上，用户只携带组名。
        "groups": groups.iter().map(|g| json!({
            "group_name": g.group_name,
            "display_name": g.display_name,
            "monthly_bytes": bytes_str(g.monthly_bytes as u128),
            "sort_order": g.sort_order,
        })).collect::<Vec<_>>(),
        "default_group": current.as_ref().map(|s| s.default_group.clone()),
        "revision": current.as_ref().map(|s| s.revision),
        "display_timezone": current.as_ref().map(|s| s.display_timezone.clone()),
        "updated_by": current.as_ref().map(|s| s.updated_by.clone()),
        "updated_at": current.as_ref().map(|s| s.updated_at.clone()),
        // 只要还有节点没应用新配置，就**不能显示「全部已生效」**。
        "fully_applied": fully,
        "nodes": tasks.iter().map(|task| json!({
            "node_id": task.node_id,
            "status": task.status.as_str(),
            "last_error": task.last_error,
        })).collect::<Vec<_>>(),
        // **不提供超发上界字段**（D20、R2）：那个数看起来有保证，实际不成立。
        "overcommit_note": "可能超发，幅度取决于下发失败时的授权额",
    })))
}

/// 一次额度变更请求。
///
/// 两种对象：`scope = "group"` 改某一档的字节数（影响该档全体），
/// `scope = "user_group"` 把某个用户换档（影响一人）。两者都可能降低某人的额度，
/// 所以共用同一套 dry_run / confirm 纪律。
///
/// 字段做成扁平的可选项而不是 serde 的内部标记枚举：`deny_unknown_fields`
/// 与 `flatten` 不能一起工作，而在一个改额度的接口上，
/// 「多打了一个字段却被静默忽略」比多几行校验代码危险得多。
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuotaUpdate {
    scope: String,
    #[serde(default)]
    group_name: Option<String>,
    /// 十进制字符串。TOML/JSON 的整数都是有符号 64 位，而额度口径是 u64（C3）。
    #[serde(default)]
    monthly_bytes: Option<String>,
    #[serde(default)]
    login_name: Option<String>,
    #[serde(default)]
    new_group: Option<String>,
    #[serde(default)]
    dry_run: bool,
    /// 调低必须先预览再确认；调高不要求。
    #[serde(default)]
    confirm: bool,
}

impl QuotaUpdate {
    fn to_scope(&self) -> ApiResult<settings::Scope> {
        match self.scope.as_str() {
            "group" => {
                let group_name = self
                    .group_name
                    .clone()
                    .ok_or_else(|| ApiError::invalid("scope=group 需要 group_name"))?;
                let bytes = self
                    .monthly_bytes
                    .as_deref()
                    .ok_or_else(|| ApiError::invalid("scope=group 需要 monthly_bytes"))?;
                Ok(settings::Scope::Group { group_name, new_monthly_bytes: parse_bytes(bytes)? })
            }
            "user_group" => {
                let login_name = self
                    .login_name
                    .clone()
                    .ok_or_else(|| ApiError::invalid("scope=user_group 需要 login_name"))?;
                let new_group = self
                    .new_group
                    .clone()
                    .ok_or_else(|| ApiError::invalid("scope=user_group 需要 new_group"))?;
                Ok(settings::Scope::UserGroup { login_name, new_group })
            }
            other => {
                Err(ApiError::invalid(format!("未知的 scope：{other}，只接受 group 或 user_group")))
            }
        }
    }
}

pub async fn put_quota_settings(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    headers: HeaderMap,
    Json(body): Json<QuotaUpdate>,
) -> ApiResult<axum::response::Response> {
    subject.require_admin()?;

    let scope = body.to_scope()?;
    let cycle = crate::quota::pool::cycle_key(OffsetDateTime::now_utc());
    let current = settings::load(&state.store).await?;

    // 乐观并发：`If-Match: <revision>`。
    if let Some(expected) = headers.get(header::IF_MATCH).and_then(|v| v.to_str().ok()) {
        let expected: i64 = expected
            .trim_matches('"')
            .parse()
            .map_err(|_| ApiError::invalid("If-Match 必须是当前 revision"))?;
        let actual = current.as_ref().map(|s| s.revision).unwrap_or(0);
        if expected != actual {
            return Err(ApiError::new(
                ApiCode::RevisionMismatch,
                "设置已被别人改过，请重新读取后再提交",
            )
            .with_detail(json!({ "expected_revision": expected, "current_revision": actual })));
        }
    }

    let preview = settings::preview(&state.store, &scope, &cycle).await?;
    if body.dry_run {
        // **只返回预览，不改设置、不写下发任务。**
        // 调用方展示结果时须核对 revision，不能把旧结果用于新设置。
        return Ok(Json(json!({
            "dry_run": true,
            "scope": body.scope,
            "revision": current.as_ref().map(|s| s.revision).unwrap_or(0),
            "current_monthly_bytes": bytes_str(preview.current_monthly_bytes as u128),
            "new_monthly_bytes": bytes_str(preview.new_monthly_bytes as u128),
            "is_reduction": preview.is_reduction,
            "affected_users": preview.affected_users(),
            // **用量核验时刻**：后续真实流量仍可能增加用尽人数。
            "usage_verified_at": preview.usage_verified_at,
            "will_be_blocked": preview.will_be_blocked.iter().map(|u| json!({
                "user_id": u.user_id,
                "login_name": u.login_name,
                "cycle_used_bytes": bytes_str(u.cycle_used),
            })).collect::<Vec<_>>(),
        }))
        .into_response());
    }

    if preview.is_reduction && !body.confirm {
        return Err(ApiError::new(
            ApiCode::ConfirmationRequired,
            "调低额度必须先 dry_run 预览再带 confirm 提交",
        )
        .with_detail(json!({ "affected_users": preview.affected_users() })));
    }

    let applied =
        settings::apply(&state.store, &scope, &subject.login_name, &cycle, body.confirm).await?;
    let tasks = settings::latest_tasks(&state.store).await?;

    // 202：**「立即生效」指主控立即采用新参数并启动重算与下发，
    // 不表示节点已确认收到新表。**
    Ok((
        StatusCode::ACCEPTED,
        Json(json!({
            "revision": applied.revision,
            "affected_users": applied.affected_users,
            "nodes": tasks.iter().map(|task| json!({
                "node_id": task.node_id,
                "status": task.status.as_str(),
            })).collect::<Vec<_>>(),
            "note": "已保存，节点待生效；调高额度不会让已经断掉的连接自己回来，用户需要自行重连",
        })),
    )
        .into_response())
}

fn parse_bytes(text: &str) -> ApiResult<u64> {
    if text.is_empty() || !text.bytes().all(|b| b.is_ascii_digit()) {
        return Err(ApiError::invalid("字节量必须是纯十进制数字串（C3）"));
    }
    text.parse().map_err(|_| ApiError::invalid("字节量超出 u64 范围"))
}

// ============ 告警（由已有数据派生） ============

/// 告警页在这一轮接**已经落库的事实**：四项判据、未闭合窗口、拒绝回执、
/// 归桶回落、409 成因。§4.10 的规则引擎与 IM 通道属于 M5。
pub async fn list_alerts(
    State(state): State<AppState>,
    subject: Subject,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let criteria = crate::collect::acceptance_criteria(&state.store).await?;

    async fn count(state: &AppState, sql: &str) -> ApiResult<i64> {
        sqlx::query(sql)
            .fetch_one(state.store.readers())
            .await
            .map(|row| row.get::<i64, _>(0))
            .map_err(|e| ApiError::internal(e.to_string()))
    }
    let unclosed =
        count(&state, "SELECT count(*) FROM node_runtimes WHERE window_state = 'unclosed'").await?;
    let rejected =
        count(&state, "SELECT count(*) FROM snapshot_batches WHERE status = 'rejected'").await?;
    let fallback =
        count(&state, "SELECT count(*) FROM snapshot_batches WHERE time_quality = 'fallback'")
            .await?;
    let conflicts =
        count(&state, "SELECT count(*) FROM quota_requests WHERE conflict_cause IS NOT NULL")
            .await?;
    let unknown_pushes =
        count(&state, "SELECT count(*) FROM quota_requests WHERE result = 'unknown'").await?;

    Ok(Json(json!({
        // 四项判据置顶——节点侧长跑里它们全为 0，放第一行等于每天复核一次结算链路。
        "acceptance_criteria": {
            "counter_regression": criteria.counter_regression,
            "unknown_runtime": criteria.unknown_runtime,
            "stale_sequence": criteria.stale_sequence,
            "unhealthy_accounted": criteria.unhealthy_accounted,
            "all_zero": criteria.all_zero(),
        },
        "counts": {
            // 未闭合窗口单独归类，**不当作正常周期切换**。
            "unclosed_windows": unclosed,
            "rejected_batches": rejected,
            "time_quality_fallback": fallback,
            "quota_conflicts": conflicts,
            "quota_unknown_results": unknown_pushes,
        },
        // §4.10 的规则引擎、限频合并与 IM 降级通道属于 M5。
        "rule_engine_enabled": false,
    })))
}

// ============ 订阅（§4.7） ============

/// 本人的订阅。**目标固定取会话主体**，`user_id` 不从请求里来。
pub async fn me_subscription(
    State(state): State<AppState>,
    subject: Subject,
) -> ApiResult<impl IntoResponse> {
    Ok(Json(subscription_view(&state, subject.user_id, &subject.login_name).await?))
}

/// 管理员看别人的订阅。
///
/// 与 CLI 的 `user subscription` 是同一件事、同一套库函数。两边都要有：
/// 控制台上做不到而命令行做得到的事，最后都会变成「找管理员上机器」，
/// 而登录依赖对外域名与 SSO 这些主控之外的事实，命令行不能因此停摆。
///
/// 这**不是**新权限：管理员本来就持有凭据文件，也本来就能跑那条命令。
pub async fn user_subscription(
    State(state): State<AppState>,
    subject: Subject,
    Path(user_id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    let row = sqlx::query("SELECT login_name FROM users WHERE user_id = ?")
        .bind(user_id)
        .fetch_optional(state.store.readers())
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    let Some(row) = row else {
        return Err(ApiError::new(ApiCode::NotFound, "没有这个用户"));
    };
    Ok(Json(subscription_view(&state, user_id, &row.get::<String, _>(0)).await?))
}

/// 两个端点共用的正文。
///
/// **GET 不签发。** 没有有效 token 就回 `url: null`——让一次查看变成一次写入，
/// 会让「谁在什么时候拿到了订阅」这条审计线索失去意义，
/// 而且任何人刷一下页面就能把自己的地址换掉。签发只发生在登录时。
async fn subscription_view(state: &AppState, user_id: i64, login_name: &str) -> ApiResult<Value> {
    let Some(config) = &state.subscription else {
        // **不是错误**：没配 `[subscription]` 是一个确定的事实，页面据此说「未启用」。
        // 回错误会被读成「加载失败，刷新试试」，而刷多少次都一样。
        return Ok(json!({ "enabled": false }));
    };

    let token = crate::subscription::current_token(&state.store, user_id)
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    let identity = crate::subscription::claimed_identity(&state.store, user_id)
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    let usage = crate::subscription::cycle_usage(&state.store, user_id)
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    let expire =
        crate::subscription::cycle_expire_unix().map_err(|e| ApiError::internal(e.to_string()))?;

    // **服务端已经知道这份订阅能不能用，就必须说出来。**
    //
    // `/sub/…` 那一侧对同样的状态是有判断的：没有身份或凭据文件里没有这个身份的
    // uPSK 时，它发的是一份 `proxies: []` 的**明说的空配置**（`src/api/sub.rs`）。
    // 而页面这一侧如果只回一个「N 条入口」的全局常数，使用者看到的就是满屏绿——
    // 他照着绿色导入、逐条试连、全部失败，再回来问「是不是你们的节点挂了」。
    // 那是本项目定义的假绿：**服务端知道，界面不说。**
    let reason = unusable_reason(state, token.as_deref(), identity.as_deref());

    Ok(json!({
        "enabled": true,
        // 这份订阅现在**拿不拿得到能用的节点**。页面上每一处绿色状态都由它决定。
        "usable": reason.is_none(),
        // 给使用者看的是文案，给运维看的是这个键——两者的处置完全不同：
        // 缺身份是容量问题，缺 uPSK 是部署问题，缺 token 是签发问题。
        "unusable_reason": reason,
        // **每一种拨法一条地址。** 这批入口是入口机上的 NAT 转发，
        // 内网与公网走的是同一条规则、同一个端口、同一份凭据、**同一个 token**——
        // 差别只有客户端拨哪个 host。所以给的是同一份订阅的两种拨法，
        // 不是两份订阅：吊销一次两条一起失效，这正是想要的。
        "subscriptions": config
            .sources
            .iter()
            .map(|source| {
                json!({
                    "prefix": source.prefix,
                    // **不回 host。** 个人端点不带连接细节（§4.11）——页面会被截图、
                    // 会被投屏，而「公网 / 内网」这句话已经够人判断该用哪一份了。
                    // 真要知道拨哪个地址，订阅正文里有，那是给客户端的。
                    "note": source.note,
                    // 客户端里显示的名字。与 `Content-Disposition` 的文件名、
                    // 订阅正文里的注释是同一个串——三处对不上时人会以为导错了订阅。
                    "profile_name":
                        crate::subscription::render::profile_name(login_name, source),
                    "url": token.as_deref().map(|token| config.subscription_url(token, source)),
                })
            })
            .collect::<Vec<_>>(),
        // 身份名不是秘密，而且它是对账时唯一能把人和节点上的条目对上的东西。
        // 没有身份时是 null——那时订阅是一份明说的空配置，不是一份坏配置。
        "identity": identity,
        // **只给显示名与落点。** 地址与端口在订阅正文里，那是给客户端的；
        // 页面上列出来只是让人知道自己有哪几条入口（§4.11）。
        "entries": config
            .entries
            .iter()
            .map(|entry| json!({ "name": entry.name, "node_id": entry.node_id }))
            .collect::<Vec<_>>(),
        "entry_count": config.entries.len(),
        "cycle": {
            // 与 `Subscription-Userinfo` 是同一组数——页面上看到的和客户端里
            // 显示的必须一致，否则「哪个是对的」会变成一个没人能回答的问题。
            "upload_bytes": bytes_str(usage.upload),
            "download_bytes": bytes_str(usage.download),
            "used_bytes": bytes_str(usage.upload + usage.download),
            "monthly_bytes": bytes_str(usage.total as u128),
            "expire_unix": expire,
        },
        "metric_scope": METRIC_SCOPE,
    }))
}

/// 一份订阅现在拿不到能用节点的理由。`None` 表示能用。
///
/// **这个集合是封闭的**，而且 `me.html` 里必须为每一个值都备着一条文案——
/// 由 `tests/m5` 的 `每一种不可用理由页面上都有对应的说明` 机械保证。
/// 少一条的后果是那个人看到一片空白：没有地址、没有解释、没有下一步。
pub const UNUSABLE_REASONS: &[&str] =
    &["no_token", "no_identity", "no_credential", "credentials_unreadable"];

fn unusable_reason(
    state: &AppState,
    token: Option<&str>,
    identity: Option<&str>,
) -> Option<String> {
    if token.is_none() {
        return Some("no_token".to_string());
    }
    let Some(identity) = identity else {
        return Some("no_identity".to_string());
    };
    match state.credentials() {
        // 凭据文件读得出来，但里面没有这个身份：**订阅正文会是空的**，
        // 而 `/sub/…` 已经为此打过一条 P1。页面上不说，等于让那条 P1 只有运维看得见。
        Ok(credentials) if !credentials.upsk.contains_key(identity) => {
            Some("no_credential".to_string())
        }
        Ok(_) => None,
        Err(error) => {
            // 与上一种**分开**：这是部署事故（文件权限、文件没了），
            // 后果也不同——那时 `/sub/…` 回 503，连下载都失败，不是下载到一份空的。
            tracing::error!(%error, "读订阅凭据失败：本人页面按不可用显示（P1）");
            Some("credentials_unreadable".to_string())
        }
    }
}

// ============ 出站目标审计（§4.8） ============

#[derive(Debug, Deserialize)]
pub struct AuditQuery {
    #[serde(default)]
    range: Option<String>,
}

/// 本人自助查（D18）。
pub async fn me_audit_access(
    State(state): State<AppState>,
    subject: Subject,
    Query(query): Query<AuditQuery>,
) -> ApiResult<impl IntoResponse> {
    audit_access(&state, subject.user_id, subject.user_id, query).await
}

/// 管理员按用户查。
///
/// **入口在用户列表每一行，不是一个独立的检索页。** §4.8 的理由很具体：
/// 从用户进比从身份名进更不容易串号——输入身份名的那种检索最容易漏掉裁剪这一步，
/// 因为身份名看起来已经足够定位了，而它恰恰不是（同一个名字可以在两个入口上
/// 分属两人）。
pub async fn user_audit_access(
    State(state): State<AppState>,
    subject: Subject,
    Path(user_id): Path<i64>,
    Query(query): Query<AuditQuery>,
) -> ApiResult<impl IntoResponse> {
    subject.require_admin()?;
    audit_access(&state, subject.user_id, user_id, query).await
}

/// 两条路由的共同实现。`actor` 是调用者，`data_subject` 是被查的人。
async fn audit_access(
    state: &AppState,
    actor: i64,
    data_subject: i64,
    query: AuditQuery,
) -> ApiResult<Json<Value>> {
    // 没配 `[audit]`：**不是 `forbidden`**——没有记录可看和不让你看是两回事（D18）。
    let Some(config) = state.audit.as_ref() else {
        return Err(ApiError::new(ApiCode::AuditNotEnabled, "出站目标审计尚未启用")
            .with_detail(json!({ "reason": "服务端没有配置 [audit]" })));
    };
    let range = match query.range.as_deref() {
        None => crate::audit::query::Range::Week,
        Some(text) => crate::audit::query::Range::parse(text)
            .ok_or_else(|| ApiError::new(ApiCode::InvalidRequest, "range 只能是 24h / 7d / 30d"))?,
    };
    let now = OffsetDateTime::now_utc();
    let now_ms = (now.unix_timestamp_nanos() / 1_000_000) as i64;

    // **拉取行为本身要落主控审计**（§4.8）：谁、什么时候、拉了谁的哪个时间窗。
    // 放文件不放库，与审计数据同一棵树、同一条纪律——也免得为一张小表去走 D32 的
    // 四步迁移。记在**取数之前**：一次失败的查询同样是一次访问。
    if let Err(error) = crate::audit::queries::record(
        &config.dir,
        &crate::audit::queries::QueryRecord {
            at: crate::ledger::bucket::to_rfc3339(now),
            actor,
            data_subject,
            range: range.as_str().to_string(),
        },
    ) {
        // 记不下来就不给看。这条查询日志是 §4.8 对「谁看过谁的明细」的全部答案，
        // 悄悄放行等于让那个答案有一个没人知道的缺口。
        tracing::error!(%error, "写审计查询日志失败：拒绝本次查询（P1）");
        return Err(ApiError::new(ApiCode::Internal, "审计查询日志不可写"));
    }

    let answer =
        crate::audit::query::access(&state.store, &config.dir, data_subject, range, now_ms)
            .await
            .map_err(|error| {
            tracing::error!(%error, "读审计镜像失败");
            ApiError::new(ApiCode::Internal, "读取审计明细失败")
        })?;
    Ok(Json(serde_json::to_value(answer).unwrap_or_else(|_| json!({}))))
}

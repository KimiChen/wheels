//! 全局额度设置与逐节点下发任务（D21、README §4.5）。
//!
//! **额度与周期都是全局的**：所有人同一个月度额度、同一个自然月周期，配一个值。
//! 没有 per-user 套餐，也就没有周期锚点、折算系数与套餐变更迁移这一整套。
//!
//! **改额度对本周期立即生效。** 额度是参数，账本记的是用量；
//! `剩余 = 额度 − 本周期已用` 本来就该跟着重算。两个方向都是期望行为：
//! 调高让已用尽的人继续用（这正是调高的目的），调低让超出新额度的人被闸断。
//!
//! 「立即生效」指**主控立即采用新参数并启动重算与下发**，
//! **不表示节点已确认收到新表**。只要还有节点没应用，对外就得说「已保存，节点待生效」。

use sqlx::Row;
use time::OffsetDateTime;

use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::store::codec::{U128Text, U64Text};
use crate::store::Store;

/// 逐节点下发任务的状态（README §4.5）。
///
/// **它不是配额请求表的状态。** 那张表记的是「某一次 PUT 的结果」，
/// 这里记的是「这个 revision 在这个节点上落到哪一步」。
/// 尤其是 `safe_zero`：零表确认成功**不冒充新配置正常生效**。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskStatus {
    Pending,
    /// 只有**按新配置计算的完整表**收到 200 才是 applied。
    Applied,
    /// 采集失败或不健康时按 C33 下发了完整零表并确认成功。
    SafeZero,
    Unknown,
    Failed,
}

impl TaskStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            TaskStatus::Pending => "pending",
            TaskStatus::Applied => "applied",
            TaskStatus::SafeZero => "safe_zero",
            TaskStatus::Unknown => "unknown",
            TaskStatus::Failed => "failed",
        }
    }

    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "pending" => Some(TaskStatus::Pending),
            "applied" => Some(TaskStatus::Applied),
            "safe_zero" => Some(TaskStatus::SafeZero),
            "unknown" => Some(TaskStatus::Unknown),
            "failed" => Some(TaskStatus::Failed),
            _ => None,
        }
    }

    /// 这个节点是不是已经按**新配置**生效了。
    ///
    /// 只有 `Applied` 算。`safe_zero` / `unknown` / `failed` 都不能显示为新配置已应用。
    pub fn is_new_config_applied(self) -> bool {
        self == TaskStatus::Applied
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct QuotaSettings {
    pub revision: i64,
    /// 新建用户的默认档位。
    pub default_group: String,
    pub display_timezone: String,
    pub updated_by: String,
    pub updated_at: String,
}

pub async fn load(store: &Store) -> Result<Option<QuotaSettings>> {
    let row = sqlx::query(
        "SELECT revision, default_group, display_timezone, updated_by, updated_at \
         FROM quota_settings WHERE id = 1",
    )
    .fetch_optional(store.readers())
    .await?;
    Ok(row.map(|row| QuotaSettings {
        revision: row.get(0),
        default_group: row.get(1),
        display_timezone: row.get(2),
        updated_by: row.get(3),
        updated_at: row.get(4),
    }))
}

/// 确保单行设置存在。**幂等**：已存在就原样返回，不覆盖任何人改过的值。
///
/// 独立于 [`apply`] 是有意的：`apply` 只负责「改」，插入初始行是「建」。
/// 合在一起会让 `apply` 里出现一条 INSERT 分支，而那条分支必须凭空
/// 编出一个 display_timezone——历史上它硬编码成了 'UTC'，
/// 与 UTC+8 的计费周期不一致，会让某一天的用量显示进错误的月份。
pub async fn ensure_initialized(store: &Store, display_timezone: &str) -> Result<()> {
    if display_timezone.trim().is_empty() {
        return Err(Error::Quota("display_timezone 不能为空：日桶按它的日历日计算".into()));
    }
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;
    sqlx::query(
        "INSERT INTO quota_settings(id, revision, default_group, display_timezone, \
         updated_by, updated_at) VALUES (1, 0, 'normal', ?, 'schema', ?) \
         ON CONFLICT(id) DO NOTHING",
    )
    .bind(display_timezone)
    .bind(&now)
    .execute(txn.conn())
    .await?;
    txn.commit().await?;
    Ok(())
}

/// 一个档位。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GroupQuota {
    pub group_name: String,
    pub display_name: String,
    pub monthly_bytes: u64,
    pub sort_order: i64,
}

/// 全部档位，按 `sort_order`。
pub async fn load_groups(store: &Store) -> Result<Vec<GroupQuota>> {
    let rows = sqlx::query(
        "SELECT group_name, display_name, monthly_bytes, sort_order FROM quota_groups \
         ORDER BY sort_order",
    )
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(GroupQuota {
                group_name: row.get(0),
                display_name: row.get(1),
                monthly_bytes: U64Text::decode(&row.get::<String, _>(2))?.get(),
                sort_order: row.get(3),
            })
        })
        .collect()
}

/// 每个**在用**用户的月度额度。规划一轮额度表时一次查出来，不在循环里逐个查。
///
/// 只收 `status = 'active'`：被禁用的人不在这张表里，规划侧因此看不到他的额度。
/// 注意这不等于「从额度表里省略他」——省略在 wire 上等于**无限额度**（C16/C30），
/// 禁用的人必须显式拿到零。那一步在 `table::assemble`。
pub async fn user_quota_map(store: &Store) -> Result<std::collections::BTreeMap<i64, u64>> {
    let rows = sqlx::query(
        "SELECT u.user_id, g.monthly_bytes FROM users u \
         JOIN quota_groups g ON g.group_name = u.quota_group \
         WHERE u.status = 'active'",
    )
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| Ok((row.get(0), U64Text::decode(&row.get::<String, _>(1))?.get())))
        .collect()
}

/// 单个用户的月度额度与档位。查不到（不存在或已禁用）时返回 `None`。
pub async fn user_quota(store: &Store, user_id: i64) -> Result<Option<(String, u64)>> {
    let row = sqlx::query(
        "SELECT u.quota_group, g.monthly_bytes FROM users u \
         JOIN quota_groups g ON g.group_name = u.quota_group \
         WHERE u.user_id = ? AND u.status = 'active'",
    )
    .bind(user_id)
    .fetch_optional(store.readers())
    .await?;
    row.map(|row| Ok((row.get(0), U64Text::decode(&row.get::<String, _>(1))?.get()))).transpose()
}

/// 一个用户在本周期的用量与闸断状态。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserUsage {
    pub user_id: i64,
    pub login_name: String,
    pub quota_group: String,
    /// 该用户所在档位的月度额度。带在这里而不是让调用方再传一个参数：
    /// 分档之后「用谁的额度判断谁」必须是不会传错的。
    pub quota_bytes: u64,
    pub cycle_used: u128,
}

impl UserUsage {
    /// 按**自己档位**的额度判断是否已用尽。
    pub fn blocked(&self) -> bool {
        self.cycle_used >= self.quota_bytes as u128
    }

    /// 假设该用户的额度变成 `quota` 时是否会被闸断。用于预览。
    pub fn blocked_at(&self, quota: u64) -> bool {
        self.cycle_used >= quota as u128
    }
}

/// 调低的预览（README §4.5：**调低必须先预览再确认**）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Preview {
    pub scope: Scope,
    pub current_monthly_bytes: u64,
    pub new_monthly_bytes: u64,
    pub is_reduction: bool,
    /// 按**当前已结算用量**预计将被闸断的名单。
    pub will_be_blocked: Vec<UserUsage>,
    /// 新配置下被闸断、旧配置下没有的。
    pub newly_blocked: Vec<UserUsage>,
    /// 旧配置下被闸断、新配置下不再的——调高的收益就体现在这里。
    pub newly_unblocked: Vec<UserUsage>,
    /// **用量核验时刻。** 预览必须带上它：
    /// 后续真实流量仍可能增加用尽人数，这个名单只对这一刻的已结算用量成立。
    pub usage_verified_at: String,
    pub cycle_key: String,
}

impl Preview {
    /// 受影响人数 = 闸断状态**发生变化**的人数。
    ///
    /// 不是「新配置下被闸断的人数」——那个数在调高时会显示成一个与本次操作
    /// 无关的存量，看的人会以为是这次改动造成的。
    pub fn affected_users(&self) -> usize {
        self.newly_blocked.len() + self.newly_unblocked.len()
    }
}

/// 本周期各用户的用量。`group` 非空时只取该档位的成员。
async fn cycle_usage(
    store: &Store,
    cycle_key: &str,
    group: Option<&str>,
) -> Result<Vec<UserUsage>> {
    let rows = sqlx::query(
        "SELECT u.user_id, u.login_name, u.quota_group, g.monthly_bytes, coalesce(t.total_bytes, ?) \
         FROM users u \
         JOIN quota_groups g ON g.group_name = u.quota_group \
         LEFT JOIN usage_cycle_totals t \
                ON t.user_id = u.user_id AND t.cycle_key = ? AND t.rule_version = ? \
         WHERE u.status = 'active' AND (? IS NULL OR u.quota_group = ?) \
         ORDER BY u.user_id",
    )
    .bind(U128Text::new(0).encode())
    .bind(cycle_key)
    .bind(crate::ledger::settle::CYCLE_RULE_VERSION)
    .bind(group)
    .bind(group)
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(UserUsage {
                user_id: row.get(0),
                login_name: row.get(1),
                quota_group: row.get(2),
                quota_bytes: U64Text::decode(&row.get::<String, _>(3))?.get(),
                cycle_used: U128Text::decode(&row.get::<String, _>(4))?.get(),
            })
        })
        .collect()
}

/// 一次额度变更的对象。
///
/// 两种动作都可能降低某人的额度，所以都走同一套预览/确认与审计纪律。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Scope {
    /// 改某一档的字节数，影响该档全体。
    Group { group_name: String, new_monthly_bytes: u64 },
    /// 把某个用户换到另一档，影响一人。
    UserGroup { login_name: String, new_group: String },
}

impl Scope {
    fn kind(&self) -> &'static str {
        match self {
            Scope::Group { .. } => "group",
            Scope::UserGroup { .. } => "user_group",
        }
    }

    fn subject(&self) -> &str {
        match self {
            Scope::Group { group_name, .. } => group_name,
            Scope::UserGroup { login_name, .. } => login_name,
        }
    }
}

async fn group_bytes(store: &Store, group_name: &str) -> Result<u64> {
    let row = sqlx::query("SELECT monthly_bytes FROM quota_groups WHERE group_name = ?")
        .bind(group_name)
        .fetch_optional(store.readers())
        .await?
        .ok_or_else(|| Error::Quota(format!("没有这个档位：{group_name}")))?;
    U64Text::decode(&row.get::<String, _>(0)).map(|v| v.get())
}

/// 预览一次额度变更。**不改设置、不写下发任务。**
pub async fn preview(store: &Store, scope: &Scope, cycle_key: &str) -> Result<Preview> {
    let (current, new_bytes, usage) = match scope {
        Scope::Group { group_name, new_monthly_bytes } => {
            let current = group_bytes(store, group_name).await?;
            // 只看这一档的成员：换成全体会把与本次操作无关的人算进受影响人数。
            let usage = cycle_usage(store, cycle_key, Some(group_name)).await?;
            (current, *new_monthly_bytes, usage)
        }
        Scope::UserGroup { login_name, new_group } => {
            let new_bytes = group_bytes(store, new_group).await?;
            let usage: Vec<UserUsage> = cycle_usage(store, cycle_key, None)
                .await?
                .into_iter()
                .filter(|u| &u.login_name == login_name)
                .collect();
            let current = match usage.first() {
                Some(user) => user.quota_bytes,
                None => {
                    return Err(Error::Quota(format!("没有这个在用用户：{login_name}")));
                }
            };
            (current, new_bytes, usage)
        }
    };

    let will_be_blocked: Vec<UserUsage> =
        usage.iter().filter(|u| u.blocked_at(new_bytes)).cloned().collect();
    let newly_blocked: Vec<UserUsage> = usage
        .iter()
        .filter(|u| u.blocked_at(new_bytes) && !u.blocked_at(current))
        .cloned()
        .collect();
    let newly_unblocked: Vec<UserUsage> = usage
        .iter()
        .filter(|u| !u.blocked_at(new_bytes) && u.blocked_at(current))
        .cloned()
        .collect();

    Ok(Preview {
        scope: scope.clone(),
        current_monthly_bytes: current,
        new_monthly_bytes: new_bytes,
        is_reduction: new_bytes < current,
        will_be_blocked,
        newly_blocked,
        newly_unblocked,
        usage_verified_at: bucket::to_rfc3339(OffsetDateTime::now_utc()),
        cycle_key: cycle_key.to_string(),
    })
}

/// 一次已提交的额度变更。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Applied {
    pub revision: i64,
    pub affected_users: usize,
    /// 逐节点待下发任务。**新配置成功应用前可重试，重启后不丢。**
    pub nodes: Vec<String>,
}

/// 提交一次额度变更。
///
/// **设置、revision、审计与待下发任务在同一写事务提交**——
/// 任何一半单独落库都会产生一个对不上的状态：
/// 设置改了但没任务 = 永远不下发；有任务但设置没改 = 推的是旧额度。
///
/// 调低必须 `confirmed = true`（调用方先看过 [`preview`]）；调高不要求。
pub async fn apply(
    store: &Store,
    scope: &Scope,
    actor: &str,
    cycle_key: &str,
    confirmed: bool,
) -> Result<Applied> {
    if actor.trim().is_empty() {
        return Err(Error::Quota("改额度必须留下操作者：这是一次影响他人的操作".into()));
    }
    let preview = preview(store, scope, cycle_key).await?;
    if preview.is_reduction && !confirmed {
        return Err(Error::Quota(format!(
            "调低额度必须先预览再确认：按 {} 的已结算用量，将有 {} 人被闸断",
            preview.usage_verified_at,
            preview.newly_blocked.len()
        )));
    }

    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let affected = preview.affected_users();
    let old_bytes = preview.current_monthly_bytes;
    let new_bytes = preview.new_monthly_bytes;
    let mut txn = store.begin_immediate().await?;

    // revision 是单条全序序列：改档位字节数与给用户换档共用它。
    let revision = sqlx::query("SELECT revision FROM quota_settings WHERE id = 1")
        .fetch_optional(txn.conn())
        .await?
        .ok_or_else(|| {
            Error::Quota("quota_settings 尚未初始化：启动时应先调用 ensure_initialized".into())
        })?
        .get::<i64, _>(0)
        .checked_add(1)
        .ok_or_else(|| Error::Quota("revision 已耗尽：非负 i64 不回绕".into()))?;

    match scope {
        Scope::Group { group_name, .. } => {
            sqlx::query(
                "UPDATE quota_groups SET monthly_bytes = ?, updated_by = ?, updated_at = ? \
                 WHERE group_name = ?",
            )
            .bind(U64Text::new(new_bytes).encode())
            .bind(actor)
            .bind(&now)
            .bind(group_name)
            .execute(txn.conn())
            .await?;
        }
        Scope::UserGroup { login_name, new_group } => {
            let changed = sqlx::query(
                "UPDATE users SET quota_group = ?, updated_at = ? \
                 WHERE login_name = ? AND status = 'active'",
            )
            .bind(new_group)
            .bind(&now)
            .bind(login_name)
            .execute(txn.conn())
            .await?
            .rows_affected();
            if changed != 1 {
                return Err(Error::Quota(format!(
                    "换档影响了 {changed} 行而不是 1 行：{login_name}"
                )));
            }
        }
    }

    sqlx::query(
        "UPDATE quota_settings SET revision = ?, updated_by = ?, updated_at = ? WHERE id = 1",
    )
    .bind(revision)
    .bind(actor)
    .bind(&now)
    .execute(txn.conn())
    .await?;

    // 落审计：谁、何时、改的是哪一档或哪个人、从多少到多少、影响多少人。
    sqlx::query(
        "INSERT INTO quota_setting_audits(revision, scope, subject, old_monthly_bytes, \
         new_monthly_bytes, affected_users, actor, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
    )
    .bind(revision)
    .bind(scope.kind())
    .bind(scope.subject())
    .bind(U64Text::new(old_bytes).encode())
    .bind(U64Text::new(new_bytes).encode())
    .bind(affected as i64)
    .bind(actor)
    .bind(&now)
    .execute(txn.conn())
    .await?;

    // 逐节点任务。只给启用了配额的节点建——没启用的节点没有闸断链路。
    let nodes: Vec<String> = sqlx::query(
        "SELECT node_id FROM nodes WHERE status = 'active' AND quota_enabled = 1 ORDER BY node_id",
    )
    .fetch_all(txn.conn())
    .await?
    .into_iter()
    .map(|row| row.get(0))
    .collect();
    for node_id in &nodes {
        sqlx::query(
            "INSERT INTO quota_update_tasks(revision, node_id, status, created_at, updated_at) \
             VALUES (?, ?, 'pending', ?, ?)",
        )
        .bind(revision)
        .bind(node_id)
        .bind(&now)
        .bind(&now)
        .execute(txn.conn())
        .await?;
    }

    txn.commit().await?;
    tracing::info!(revision, actor, affected, nodes = nodes.len(), "额度已变更，逐节点任务已生成");
    Ok(Applied { revision, affected_users: affected, nodes })
}

/// 更新一个节点任务的状态。
pub async fn set_task_status(
    store: &Store,
    revision: i64,
    node_id: &str,
    status: TaskStatus,
    last_error: Option<&str>,
) -> Result<()> {
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;
    sqlx::query(
        "UPDATE quota_update_tasks SET status = ?, last_error = ?, updated_at = ? \
         WHERE revision = ? AND node_id = ?",
    )
    .bind(status.as_str())
    .bind(last_error)
    .bind(&now)
    .bind(revision)
    .bind(node_id)
    .execute(txn.conn())
    .await?;
    txn.commit().await?;
    Ok(())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NodeTask {
    pub revision: i64,
    pub node_id: String,
    pub status: TaskStatus,
    pub last_error: Option<String>,
}

/// **最新 revision** 的逐节点任务。
///
/// 连续修改时旧任务不能恢复旧额度，所以收敛循环只看最新 revision——
/// 旧 revision 的任务留在库里作为记录，但不再驱动下发。
pub async fn latest_tasks(store: &Store) -> Result<Vec<NodeTask>> {
    let rows = sqlx::query(
        "SELECT revision, node_id, status, last_error FROM quota_update_tasks \
         WHERE revision = (SELECT MAX(revision) FROM quota_update_tasks) ORDER BY node_id",
    )
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(NodeTask {
                revision: row.get(0),
                node_id: row.get(1),
                status: TaskStatus::parse(&row.get::<String, _>(2))
                    .ok_or_else(|| Error::Quota("任务状态不合法".into()))?,
                last_error: row.get(3),
            })
        })
        .collect()
}

/// 最新 revision 是不是**全部节点都按新配置生效了**。
///
/// 只要还有节点没 `applied`，对外就得说「已保存，节点待生效」及原因，
/// **不能显示「全部已生效」**。
pub async fn fully_applied(store: &Store) -> Result<bool> {
    let tasks = latest_tasks(store).await?;
    Ok(!tasks.is_empty() && tasks.iter().all(|t| t.status.is_new_config_applied()))
}

/// 审计记录。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AuditEntry {
    pub revision: i64,
    pub old_monthly_bytes: u64,
    pub new_monthly_bytes: u64,
    pub affected_users: i64,
    pub actor: String,
    pub created_at: String,
}

pub async fn audit_log(store: &Store) -> Result<Vec<AuditEntry>> {
    let rows = sqlx::query(
        "SELECT revision, old_monthly_bytes, new_monthly_bytes, affected_users, actor, created_at \
         FROM quota_setting_audits ORDER BY revision",
    )
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(AuditEntry {
                revision: row.get(0),
                old_monthly_bytes: U64Text::decode(&row.get::<String, _>(1))?.get(),
                new_monthly_bytes: U64Text::decode(&row.get::<String, _>(2))?.get(),
                affected_users: row.get(3),
                actor: row.get(4),
                created_at: row.get(5),
            })
        })
        .collect()
}

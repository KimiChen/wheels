//! 三条命令的实现（README §4.2）。
//!
//! 两条 agent 侧的纪律贯穿这里：
//!
//! - **agent 不解析快照 JSON、不缓存快照**（README §4.1）。`snapshot` 与 `quota`
//!   都是字节透传：解析属于主控，缓存会让「谁是真相源」这件事出现第二个答案。
//! - **硬校验放在 agent 侧**，因为它是唯一知道本机真实布局的一方。
//!   `audit-fetch` 拒绝读活动文件这条，只有 agent 判得了。

use std::path::Path;
use std::sync::Arc;

use proxy_manager_wire::audit;
use proxy_manager_wire::command::{AgentCommand, AuditFetchRequest};
use proxy_manager_wire::uds::UdsClient;
use tokio::sync::Semaphore;

use crate::config::AgentConfig;

/// 命令执行的结果。
///
/// `upstream_status` 是**上游**（本机 UDS）的状态码，随响应头回传给主控。
/// agent 自身的 HTTP 状态只表示「这条命令有没有被执行」，两者不能混。
pub struct Outcome {
    pub agent_status: u16,
    pub upstream_status: Option<u16>,
    pub body: Vec<u8>,
}

impl Outcome {
    fn agent_error(status: u16) -> Self {
        Outcome { agent_status: status, upstream_status: None, body: Vec::new() }
    }
}

pub struct Executor {
    config: AgentConfig,
    client: UdsClient,
    /// **单飞**：同一节点同一时刻只允许一条 in-flight 采集，第二条直接本地 429，
    /// 不去撞节点的并发上限（README §4.2）。
    ///
    /// C7 的 429 是「可重试且不得入账」，本地挡掉能减少这类可重试事件——
    /// 撞到节点的并发上限同样是 429，但那一份会占掉节点的一个连接槽。
    snapshot_slot: Arc<Semaphore>,
}

impl Executor {
    pub fn new(config: AgentConfig) -> Self {
        Executor {
            config,
            client: UdsClient::default(),
            snapshot_slot: Arc::new(Semaphore::new(1)),
        }
    }

    pub fn node_id(&self) -> &str {
        &self.config.node_id
    }

    pub async fn execute(&self, command: &AgentCommand) -> Outcome {
        match command {
            AgentCommand::Snapshot {} => self.snapshot().await,
            AgentCommand::Quota { body } => self.quota(body).await,
            AgentCommand::AuditFetch(request) => self.audit_fetch(request).await,
        }
    }

    async fn snapshot(&self) -> Outcome {
        let Ok(_permit) = self.snapshot_slot.clone().try_acquire_owned() else {
            // 本地 429。与上游的 429 在 wire 上是可区分的：
            // 这里没有 upstream_status，那里有。两者对采集端的处置相同
            // （可重试且不得入账），但归因完全不同。
            tracing::debug!("快照单飞：已有一条在途，本地 429");
            return Outcome::agent_error(429);
        };
        match self.client.get(&self.config.sockets.snapshot, "/v3/snapshot").await {
            // 字节透传：不解析、不重序列化。重序列化会改变字段顺序与数字表示，
            // 而主控要对**解压后的原始响应字节**算摘要（data-model.md §5）。
            Ok(response) => Outcome {
                agent_status: 200,
                upstream_status: Some(response.status),
                body: response.body,
            },
            Err(error) => {
                tracing::warn!(error = %error, "快照采集失败");
                Outcome::agent_error(502)
            }
        }
    }

    async fn quota(&self, body: &[u8]) -> Outcome {
        // 原样 PUT。agent 不校验这个 body——全量表的语义属于主控与数据面之间的合同，
        // 在中间多做一次校验只会产生第三份可能漂移的定义。
        match self.client.put_json(&self.config.sockets.quota, "/v3/quota", body).await {
            Ok(response) => Outcome {
                agent_status: 200,
                upstream_status: Some(response.status),
                body: response.body,
            },
            Err(error) => {
                tracing::warn!(error = %error, "配额下发失败");
                Outcome::agent_error(502)
            }
        }
    }

    async fn audit_fetch(&self, request: &AuditFetchRequest) -> Outcome {
        let Some(dir) = self.config.audit_dir.as_ref() else {
            tracing::warn!("未配置 audit_dir，拒绝审计检索");
            return Outcome::agent_error(404);
        };
        match read_rotated(dir, &request.identity) {
            Ok(body) => Outcome { agent_status: 200, upstream_status: Some(200), body },
            Err(error) => {
                // 日志里不记身份明细（threat-model §5），只记错误码。
                tracing::warn!(error = %error, "审计检索失败");
                Outcome::agent_error(500)
            }
        }
    }
}

/// 只读**已轮转**文件（C23）。
///
/// 两条都要守住：
///
/// - **拒绝读取活动文件。** 它正在被写，读到的可能是半行；而且 §4.8 已经接受了
///   「低流量身份可能数天拿不到归档」这个限制——用读活动文件去绕开它，
///   等于把一个已披露的限制换成一个不可见的正确性问题。
/// - **agent 自身从不 `rm` 活动文件。** 轮转只许 `mv`；要删某人历史，
///   删的是已轮转文件不是活动文件。这个函数只读，不删任何东西。
fn read_rotated(dir: &Path, identity: &str) -> std::io::Result<Vec<u8>> {
    let mut names: Vec<String> = Vec::new();
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().to_string();
        if audit::is_active_file(&name, identity) {
            // 显式跳过，并留一条痕迹：这不是「没找到」，是「刻意不读」。
            tracing::debug!("跳过活动文件：只读已轮转文件（C23）");
            continue;
        }
        if audit::is_rotated_file(&name, identity) {
            names.push(name);
        }
    }
    // 排序让结果确定。文件名末尾是时间戳，字典序即时间序。
    names.sort();

    let mut body = Vec::new();
    for name in names {
        body.extend_from_slice(&std::fs::read(dir.join(name))?);
    }
    Ok(body)
}

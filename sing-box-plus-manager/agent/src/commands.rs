//! 三条命令的实现（README §4.2）。
//!
//! 两条 agent 侧的纪律贯穿这里：
//!
//! - **agent 不解析快照 JSON、不缓存快照**（README §4.1）。`snapshot` 与 `quota`
//!   都是字节透传：解析属于主控，缓存会让「谁是真相源」这件事出现第二个答案。
//! - **硬校验放在 agent 侧**，因为它是唯一知道本机真实布局的一方。
//!   `audit-read` 拒绝读活动文件、拒绝走出 `audit_dir` 这两条，只有 agent 判得了。

use std::path::Path;
use std::sync::Arc;

use proxy_manager_wire::audit;
use proxy_manager_wire::command::{AgentCommand, AuditFileEntry, AuditListing, AuditReadRequest};
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
            AgentCommand::AuditList {} => self.audit_list().await,
            AgentCommand::AuditRead(request) => self.audit_read(request).await,
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

    async fn audit_list(&self) -> Outcome {
        let Some(dir) = self.config.audit_dir.as_ref() else {
            tracing::warn!("未配置 audit_dir，拒绝审计检索");
            return Outcome::agent_error(404);
        };
        match list_rotated(dir) {
            Ok(listing) => {
                if listing.skipped > 0 {
                    // 计数而不是静默跳过：真跳过了东西却不说，与「目录里就这些」
                    // 在输出上长得一模一样。
                    tracing::warn!(skipped = listing.skipped, "审计目录里有无法归类的条目");
                }
                match serde_json::to_vec(&listing) {
                    Ok(body) => Outcome { agent_status: 200, upstream_status: Some(200), body },
                    Err(error) => {
                        tracing::warn!(error = %error, "审计清单序列化失败");
                        Outcome::agent_error(500)
                    }
                }
            }
            Err(error) => {
                // 日志里不记身份明细（threat-model §5），只记错误码。
                tracing::warn!(error = %error, "审计目录列举失败");
                Outcome::agent_error(500)
            }
        }
    }

    async fn audit_read(&self, request: &AuditReadRequest) -> Outcome {
        let Some(dir) = self.config.audit_dir.as_ref() else {
            tracing::warn!("未配置 audit_dir，拒绝审计检索");
            return Outcome::agent_error(404);
        };
        match read_rotated(dir, &request.file) {
            Ok(body) => Outcome { agent_status: 200, upstream_status: Some(200), body },
            Err(error) => {
                // 三档分开：400 是「这个名字我不接受」，404 是「名字没问题但文件不在了」，
                // 500 才是 agent 自己出了问题。同步器要靠这个区分「节点删了文件」
                // 与「agent 坏了」——混成一个码，前者会被当成后者反复重试。
                let status = match error.kind() {
                    std::io::ErrorKind::InvalidInput => 400,
                    std::io::ErrorKind::NotFound => 404,
                    _ => 500,
                };
                // 不把被拒的文件名回显进日志：它来自主控，但这条路径的全部意义
                // 就是不信任它。
                tracing::warn!(error = %error, status, "审计读取被拒或失败");
                Outcome::agent_error(status)
            }
        }
    }
}

/// 列出目录里**全部身份**的已轮转文件（C23）。
///
/// 活动文件**不计入 `skipped`**：跳过它是这个函数的本职（它正在被写，
/// 读到的可能是半行），把本职算成异常会让 `skipped` 这个数字失去意义。
/// `skipped` 数的是**无法归类**的条目——那才是「这个目录里有我说不清的东西」。
fn list_rotated(dir: &Path) -> std::io::Result<AuditListing> {
    let mut files = Vec::new();
    let mut skipped = 0usize;
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().to_string();
        if !audit::is_rotated_name(&name) {
            if !name.starts_with(audit::FILE_PREFIX) || !name.ends_with(audit::FILE_SUFFIX) {
                skipped += 1;
            }
            continue;
        }
        // 只要常规文件。目录与符号链接都不读——后者可能指到 audit_dir 之外。
        let meta = entry.metadata()?;
        if !meta.is_file() {
            skipped += 1;
            continue;
        }
        files.push(AuditFileEntry { name, size: meta.len() });
    }
    // 定宽时间戳的字典序即时间序，所以排序之后就是轮转顺序。
    files.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(AuditListing { files, skipped })
}

/// 读**一个**已轮转文件（C23）。
///
/// 三条都要守住：
///
/// - **拒绝读取活动文件。** 它正在被写，读到的可能是半行；而且 §4.8 已经接受了
///   「低流量身份可能数天拿不到归档」这个限制——用读活动文件去绕开它，
///   等于把一个已披露的限制换成一个不可见的正确性问题。
/// - **文件名来自主控，但这里不信任它。** 形状校验 + canonicalize 之后再确认
///   仍在 `audit_dir` 之内。前者挡 `../`，后者挡符号链接。
/// - **agent 自身从不 `rm` 任何东西。** 轮转只许 `mv`；要删某人历史，
///   删的是已轮转文件不是活动文件。这个函数只读。
fn read_rotated(dir: &Path, file: &str) -> std::io::Result<Vec<u8>> {
    let refuse = |why: &str| {
        Err::<Vec<u8>, _>(std::io::Error::new(std::io::ErrorKind::InvalidInput, why.to_string()))
    };
    // 第一道：形状。身份段只许 base64url 字母表（里面没有 `/` 也没有 `.`），
    // 所以 `../../etc/passwd` 这一类根本拼不出来；活动文件也在这一道被挡掉——
    // 它没有轮转时间戳后缀。
    if !audit::is_rotated_name(file) {
        return refuse("不是合法的已轮转审计文件名");
    }
    // 第二道：拼出来之后确认还在 audit_dir 之内。形状那一道已经够了，
    // 但这一道挡的是另一类东西——目录里放了一个符号链接指到外面去。
    // canonicalize 会解析链接，所以两道都得过。
    let path = dir.join(file);
    let (real_dir, real_path) = (dir.canonicalize()?, path.canonicalize()?);
    if !real_path.starts_with(&real_dir) {
        return refuse("解析之后不在 audit_dir 之内");
    }
    if !real_path.symlink_metadata()?.is_file() {
        return refuse("不是常规文件");
    }
    std::fs::read(&real_path)
}

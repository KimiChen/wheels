//! 把节点审计目录同步到本机镜像。
//!
//! # 轮转的可靠信号
//!
//! 活动文件按字节偏移增量读。轮转会把同名文件变回 0 字节，于是很自然会想用
//! 「文件比我记的偏移还短」当轮转信号——**那是错的**。同步是周期性的：
//! 按线上实测的 8.4 KB/分，10 分钟一轮之间新文件能长到 84 KB，
//! 而旧偏移可能只有 50 KB。那时条件不成立，我们会从 50 KB 处接着读一个
//! **新**文件，静默跳过它的前 50 KB。这种漏读不会报错，也不会在任何计数上体现。
//!
//! 可靠信号是**这个身份出现了更新的归档**：每一次轮转恰好产生一个归档文件，
//! 而归档文件不会再变。比较用字典序——轮转时间戳是定宽的，所以字典序即时间序
//! （`wire::audit::is_rotation_stamp`）。这也是为什么那个定宽性质值得单独立一条用例。
//!
//! 「文件变短但没有新归档」是**说不清**的状态，不猜：记一条异常并停住这个文件。
//! 停必须是粘的——文件会长回去，条件会自己消失，而数据已经漏了。
//!
//! # 不删任何东西
//!
//! 使用者已定：拉回来的文件只增不删，先看真实增量。这里唯一会缩短本机文件的动作
//! 是轮转时清空 live 副本，而那**只在对应归档已经落到本机之后**才做——
//! 归档完整覆盖了 live 副本的内容，所以那不是删除，是去重。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};
use proxy_manager_wire::audit;
use proxy_manager_wire::command::AuditListing;

/// 节点侧审计数据的来源。抽成 trait 只为一件事：轮转与偏移的逻辑要能在没有
/// 真节点的情况下测——那些分支恰恰是最难在真机上复现的。
pub trait Source {
    fn list(&self) -> impl std::future::Future<Output = Result<AuditListing>> + Send;
    fn read(
        &self,
        file: &str,
        offset: u64,
    ) -> impl std::future::Future<Output = Result<Vec<u8>>> + Send;
}

/// 每个活动文件的同步状态。
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct ActiveState {
    /// 本机 live 副本已经收下的字节数，也就是下一次该从哪儿接着读。
    pub offset: u64,
    /// 开始累积当前这份 live 副本时，本机持有的该身份**最新归档**的文件名。
    ///
    /// 轮转判定就是拿它和当下的最新归档比。`None` 表示那时一份归档都没有——
    /// 于是第一份归档出现时会正确地触发一次归零。
    #[serde(default)]
    pub archive_high_water: Option<String>,
    /// 「变短但没有新归档」这种说不清的状态。**粘住**，直到一份新归档把它解开。
    #[serde(default)]
    pub anomaly: bool,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct SyncState {
    #[serde(default)]
    active: BTreeMap<String, ActiveState>,
}

/// 一台节点在本机的镜像目录。
pub struct NodeMirror {
    dir: PathBuf,
}

const STATE_FILE: &str = ".sync-state.json";

impl NodeMirror {
    /// `root` 是审计树根，通常是 `/var/lib/proxy-manager/audit`。
    pub fn new(root: &Path, node_id: &str) -> Result<Self> {
        // node_id 来自本机配置，不是网络输入；但它会被拼进路径，所以还是挡一道。
        if node_id.is_empty() || node_id.contains('/') || node_id.contains("..") {
            return Err(Error::Audit(format!("node_id 不能用作目录名：{node_id}")));
        }
        let dir = root.join(node_id);
        std::fs::create_dir_all(&dir).map_err(|e| Error::Audit(format!("建审计镜像目录：{e}")))?;
        Ok(NodeMirror { dir })
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    fn load_state(&self) -> Result<SyncState> {
        let path = self.dir.join(STATE_FILE);
        match std::fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map_err(|e| Error::Audit(format!("解析 {}：{e}", path.display()))),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(SyncState::default()),
            Err(error) => Err(Error::Audit(format!("读 {}：{error}", path.display()))),
        }
    }

    fn save_state(&self, state: &SyncState) -> Result<()> {
        let body = serde_json::to_vec_pretty(state)
            .map_err(|e| Error::Audit(format!("序列化同步状态：{e}")))?;
        self.write_atomic(STATE_FILE, &body)
    }

    /// 先写 `.partial` 再改名。**中途失败时留下的是一个名字就说明它没写完的文件**，
    /// 而不是一份看起来正常、其实截断了的归档。
    fn write_atomic(&self, name: &str, body: &[u8]) -> Result<()> {
        let partial = self.dir.join(format!("{name}.partial"));
        std::fs::write(&partial, body)
            .map_err(|e| Error::Audit(format!("写 {}：{e}", partial.display())))?;
        std::fs::rename(&partial, self.dir.join(name))
            .map_err(|e| Error::Audit(format!("改名 {}：{e}", partial.display())))
    }

    fn append(&self, name: &str, body: &[u8]) -> Result<()> {
        use std::io::Write;
        let path = self.dir.join(name);
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .map_err(|e| Error::Audit(format!("打开 {}：{e}", path.display())))?;
        file.write_all(body).map_err(|e| Error::Audit(format!("追加 {}：{e}", path.display())))
    }

    fn truncate(&self, name: &str) -> Result<()> {
        let path = self.dir.join(name);
        match std::fs::File::create(&path) {
            Ok(_) => Ok(()),
            Err(error) => Err(Error::Audit(format!("清空 {}：{error}", path.display()))),
        }
    }

    /// 本机已持有的文件名 → 大小。只看审计文件，`.partial` 与状态文件都不算。
    fn held(&self) -> Result<BTreeMap<String, u64>> {
        let mut out = BTreeMap::new();
        for entry in std::fs::read_dir(&self.dir)
            .map_err(|e| Error::Audit(format!("列 {}：{e}", self.dir.display())))?
        {
            let entry = entry.map_err(|e| Error::Audit(format!("列目录条目：{e}")))?;
            let name = entry.file_name().to_string_lossy().to_string();
            if audit::name_kind(&name).is_none() {
                continue;
            }
            let meta =
                entry.metadata().map_err(|e| Error::Audit(format!("取 {name} 属性：{e}")))?;
            if meta.is_file() {
                out.insert(name, meta.len());
            }
        }
        Ok(out)
    }
}

/// 一轮同步的结果。字段都是**可核对的数**，不是「成功/失败」。
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Report {
    pub node_id: String,
    /// 这一轮新拉到的归档文件数与字节数。
    pub archives_fetched: usize,
    pub archive_bytes: u64,
    /// 这一轮从活动文件增量收下的字节数。
    pub live_bytes: u64,
    /// 检测到的轮转次数。
    pub rotations: usize,
    /// 节点侧列目录时无法归类的条目数，原样带上来。
    pub skipped_on_node: usize,
    /// 说不清因而被停住的活动文件。**停是粘的**，所以它会一直报到被解开为止。
    pub anomalies: Vec<String>,
    /// 这一轮里单个文件的失败。一个文件失败不影响别的文件。
    pub errors: Vec<String>,
}

impl Report {
    pub fn clean(&self) -> bool {
        self.anomalies.is_empty() && self.errors.is_empty()
    }
}

fn segment_of(name: &str) -> Option<&str> {
    audit::name_kind(name).map(|(_, segment)| segment)
}

/// 同步一台节点。
pub async fn sync_node<S: Source>(
    node_id: &str,
    source: &S,
    mirror: &NodeMirror,
) -> Result<Report> {
    let mut report = Report { node_id: node_id.to_string(), ..Report::default() };
    let listing = source.list().await?;
    report.skipped_on_node = listing.skipped;
    let mut state = mirror.load_state()?;
    let mut held = mirror.held()?;

    // 一、先补齐归档。必须排在活动文件之前：轮转判定要看「本机是否已持有更新的归档」，
    // 而清空 live 副本的前提是那份归档已经安全落地。
    for entry in listing.files.iter().filter(|f| !f.active) {
        if held.get(&entry.name) == Some(&entry.size) {
            continue;
        }
        match source.read(&entry.name, 0).await {
            Ok(body) if body.len() as u64 != entry.size => {
                // 清单报的大小与真读到的对不上：要么拉了一半，要么文件在两次调用之间
                // 变过。归档是不变的，所以这两种都不该发生——记下来，下一轮重试。
                report.errors.push(format!(
                    "{}：清单报 {} 字节，读到 {} 字节",
                    entry.name,
                    entry.size,
                    body.len()
                ));
            }
            Ok(body) => {
                mirror.write_atomic(&entry.name, &body)?;
                held.insert(entry.name.clone(), body.len() as u64);
                report.archives_fetched += 1;
                report.archive_bytes += body.len() as u64;
            }
            Err(error) => report.errors.push(format!("{}：{error}", entry.name)),
        }
    }

    // 二、活动文件按偏移增量读。
    for entry in listing.files.iter().filter(|f| f.active) {
        let Some(segment) = segment_of(&entry.name) else { continue };
        let current = held
            .keys()
            .filter(|name| {
                segment_of(name) == Some(segment)
                    && audit::name_kind(name).map(|(k, _)| k) == Some(audit::NameKind::Rotated)
            })
            .max()
            .cloned();
        let slot = state.active.entry(entry.name.clone()).or_default();

        if current > slot.archive_high_water {
            // 轮转了，而且对应的归档已经在本机——它完整覆盖了 live 副本，
            // 所以清空 live 副本不是删除，是去重。
            mirror.truncate(&entry.name)?;
            slot.offset = 0;
            slot.archive_high_water = current;
            slot.anomaly = false;
            report.rotations += 1;
        } else if slot.anomaly || entry.size < slot.offset {
            // 变短却没有新归档，说不清。**不猜**——停住这个文件并报出来。
            // 停必须是粘的：文件会长回去，条件会自己消失，而那时数据已经漏了。
            slot.anomaly = true;
            report.anomalies.push(format!(
                "{}：线上 {} 字节 < 已收 {} 字节，却没有更新的归档",
                entry.name, entry.size, slot.offset
            ));
            continue;
        }

        match source.read(&entry.name, slot.offset).await {
            Ok(body) if body.is_empty() => {}
            Ok(body) => {
                mirror.append(&entry.name, &body)?;
                // **推进量是真正收下的字节数**，不是请求的范围长度：
                // agent 会把活动文件截到最后一个完整行。
                slot.offset += body.len() as u64;
                report.live_bytes += body.len() as u64;
            }
            Err(error) => report.errors.push(format!("{}：{error}", entry.name)),
        }
    }

    mirror.save_state(&state)?;
    Ok(report)
}

/// 真通路。
///
/// 这里没有任何重试或退避：一个文件失败就记一条错误，下一轮自己补——归档不会变化，
/// 活动文件按偏移增量，两者都天然可续。重试逻辑放在这一层只会让「这一轮到底收下了
/// 什么」变得说不清。
impl Source for crate::agent::AgentClient {
    async fn list(&self) -> Result<AuditListing> {
        let response = self.audit_list().await?;
        if response.agent_status != 200 {
            return Err(Error::Audit(format!("列审计目录：agent 回 {}", response.agent_status)));
        }
        serde_json::from_slice(&response.body)
            .map_err(|e| Error::Audit(format!("解析审计清单：{e}")))
    }

    async fn read(&self, file: &str, offset: u64) -> Result<Vec<u8>> {
        let response = self.audit_read(file.to_string(), offset).await?;
        match response.agent_status {
            200 => Ok(response.body),
            // 404 是「文件不在了」，不是通路故障。总量封顶会删最老的归档，
            // 那时节点自己会往对应身份的文件里写一条 ev=gap/total_cap，
            // 所以这个缺口在数据里看得见，不需要在这里编一个。
            404 => Err(Error::Audit(format!("{file}：节点上已经没有这个文件"))),
            status => Err(Error::Audit(format!("{file}：agent 回 {status}"))),
        }
    }
}

/// 对配置里的每一台节点同步一轮。
///
/// 一台失败不影响别的：审计是**每台节点独立**的一棵子树，把一台的通路故障变成
/// 整轮失败，只会让一台坏机器挡住其余三台的数据。
pub async fn sync_all(
    nodes: &[crate::config::NodeConfig],
    audit: &crate::config::AuditConfig,
) -> Vec<std::result::Result<Report, String>> {
    let mut out = Vec::new();
    for node in nodes {
        out.push(sync_one(node, audit).await.map_err(|e| format!("{}：{e}", node.node_id)));
    }
    out
}

async fn sync_one(
    node: &crate::config::NodeConfig,
    audit: &crate::config::AuditConfig,
) -> Result<Report> {
    let client = crate::agent::AgentClient::new(
        &node.node_id,
        &node.address,
        node.dns_name(),
        &node.materials_dir,
    )?;
    let mirror = NodeMirror::new(&audit.dir, &node.node_id)?;
    sync_node(&node.node_id, &client, &mirror).await
}

/// 定时循环。收到停止信号即退出。
pub async fn run(
    nodes: Vec<crate::config::NodeConfig>,
    audit: crate::config::AuditConfig,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) {
    let period = std::time::Duration::from_secs(audit.interval_secs as u64);
    loop {
        for outcome in sync_all(&nodes, &audit).await {
            match outcome {
                Ok(report) if report.clean() => tracing::info!(
                    node_id = %report.node_id,
                    archives = report.archives_fetched,
                    live_bytes = report.live_bytes,
                    rotations = report.rotations,
                    "审计同步完成"
                ),
                Ok(report) => tracing::warn!(
                    node_id = %report.node_id,
                    anomalies = ?report.anomalies,
                    errors = ?report.errors,
                    "审计同步有未决项"
                ),
                Err(detail) => tracing::warn!(detail, "审计同步失败"),
            }
        }
        tokio::select! {
            _ = tokio::time::sleep(period) => {}
            _ = shutdown.changed() => {
                if *shutdown.borrow() {
                    tracing::info!("收到停止信号，退出审计同步循环");
                    return;
                }
            }
        }
    }
}

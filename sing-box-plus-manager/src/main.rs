//! proxy-manager 主控二进制入口。
//!
//! 已实现的部分：配置解析与建库（M0）、双 CA PKI 的离线签发 / 吊销 / 巡检（M1）。
//! 采集调度、配额下发与 HTTP 服务分别属于 M1 的后半、M2 与 M4，
//! **不在这里放空壳**——一个「起得来但什么都不做」的服务会让
//! C22「退出码不是健康证据」立刻兑现。
//!
//! PKI 子命令跑在**离线 workspace** 上：root 与全部 intermediate 私钥留在那里，
//! 节点只拿 `deployment/agent/` 那一层（`docs/threat-model.md` §2.4）。

use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use proxy_manager::config::Config;
use proxy_manager::pki::{self, inspect, issue, Workspace};
use proxy_manager::store::Store;

#[derive(Parser)]
#[command(name = "proxy-manager", about = "sing-box-plus 节点的采集、结算、配额与控制台主控")]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// 解析配置并建库。
    Run {
        /// 配置目录，默认 /etc/proxy-manager。
        #[arg(default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
    /// 双 CA PKI 的离线操作。
    Pki {
        #[command(subcommand)]
        command: PkiCommand,
    },
    /// 采集侧的排查工具。
    Collect {
        #[command(subcommand)]
        command: CollectCommand,
    },
    /// runtime 窗口的登记与批准。
    Runtime {
        #[command(subcommand)]
        command: RuntimeCommand,
    },
    /// 账本操作。
    Ledger {
        #[command(subcommand)]
        command: LedgerCommand,
    },
}

#[derive(Subcommand)]
enum RuntimeCommand {
    /// 列出已登记的 runtime 窗口。
    List {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
    /// 列出**收到过快照但还没登记**的 runtime——影子 runtime 的可见形态。
    Pending {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
    /// 登记并批准一个 runtime。
    ///
    /// 首快照策略必须显式选：`baseline` 把节点已有累计当作起点，
    /// `include` 把它一次性记成本周期用量。**选 include 必须能证明
    /// 不存在其他数据源的重复入账**，不能因为计数从零开始就隐式批准。
    Approve {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        node: String,
        #[arg(long)]
        runtime: String,
        /// 该 runtime 快照里的 `started_at_unix_ms`。首次确认后固定不变。
        #[arg(long)]
        started_at_ms: u64,
        #[arg(long, value_parser = ["baseline", "include"])]
        policy: String,
        /// 为什么批准。与操作者一起留档——只留策略等于没留。
        #[arg(long)]
        reason: String,
        #[arg(long)]
        actor: String,
    },
}

#[derive(Subcommand)]
enum LedgerCommand {
    /// 接收一份快照文件并结算一次。
    ///
    /// 用于按已捕获的原始字节重放——C5 的失败关闭要人工介入，
    /// 而复盘的输入就是这些字节。
    Ingest {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        node: String,
        /// 快照文件（响应体，或含响应头的完整往返记录）。
        file: PathBuf,
    },
    /// 报告 README §1 验收定义里的**四项判据**。长跑时它们必须全为 0。
    ///
    /// 从**库里**统计而不是进程内计数器：计数器随重启清零，
    /// 而长跑的结论要跨重启成立。
    Stats {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
}

#[derive(Subcommand)]
enum CollectCommand {
    /// 按 v3 契约校验一份快照，并说明**为什么**不认。
    ///
    /// 这是排查「采集器拒绝了真实快照」时的第一站。`docs/integration-contract.md` §4
    /// 记着一次真实的漂移：主控把版本断言写成 `2`，于是拒绝每一份真实快照，
    /// 而失败信息把排查方向引到了节点侧去。有这个子命令，那次排查会短得多。
    CheckSnapshot {
        /// 快照文件。可以是 `GET /v3/snapshot` 的**响应体**，
        /// 也可以是含响应头的完整往返记录（会自动剥掉头并顺带校验 Content-Length）。
        path: PathBuf,
    },
}

#[derive(Subcommand)]
enum PkiCommand {
    /// 建离线 workspace 并生成 root CA。**不覆盖既有私钥。**
    Init {
        #[arg(long, default_value = "pki-workspace")]
        workspace: PathBuf,
    },
    /// 为一个节点签发一个 generation 的完整材料。
    ///
    /// generation 用日期 + 序号命名（如 `2026-09-18-a`）：
    /// 轮换时新旧并存一段时间，这是 `overlap → 换叶 → revoke` 里 overlap 那步的前提。
    IssueNode {
        #[arg(long)]
        node: String,
        #[arg(long)]
        generation: String,
        /// server leaf 的 DNS SAN。签发阶段精确匹配。
        #[arg(long)]
        dns: String,
        #[arg(long, default_value = "pki-workspace")]
        workspace: PathBuf,
        /// leaf 有效期（天）。CA 长、leaf 短：轮换的是叶子。
        #[arg(long, default_value_t = 90)]
        leaf_days: i64,
    },
    /// 到期巡检。**只读公钥叶子**，输出不含 PEM、私钥或证书主题。
    Inspect {
        /// 公证书路径。只接受 `*.pem`——巡检不该有理由去碰私钥。
        paths: Vec<PathBuf>,
        #[arg(long, default_value_t = 30)]
        warning_days: i64,
        #[arg(long, default_value_t = 7)]
        critical_days: i64,
    },
    /// 吊销：**从授权 bundle 里移除**指定指纹的 leaf。
    ///
    /// 证书上的 `REVOKED` 标记只是离线审计记录，不会让任何在线组件拒绝谁。
    /// 三阶段顺序不能反：`overlap → 换叶 → revoke`，reload **先节点后主控**——
    /// 反过来会在切换窗口里把自己关在门外。
    Revoke {
        #[arg(long)]
        bundle: PathBuf,
        /// 要移除的 leaf 的 SPKI SHA-256（64 位十六进制）。
        #[arg(long)]
        spki: String,
    },
}

#[tokio::main]
async fn main() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let cli = Cli::parse();
    let command =
        cli.command.unwrap_or(Command::Run { config_dir: PathBuf::from("/etc/proxy-manager") });

    match run(command).await {
        Ok(code) => code,
        Err(error) => {
            tracing::error!("失败：{error}");
            ExitCode::FAILURE
        }
    }
}

async fn run(command: Command) -> anyhow::Result<ExitCode> {
    match command {
        Command::Run { config_dir } => run_service(&config_dir).await,

        Command::Pki { command } => pki_command(command),

        Command::Collect { command } => collect_command(command),

        Command::Runtime { command } => runtime_command(command).await,

        Command::Ledger { command } => ledger_command(command).await,
    }
}

/// 常驻服务：每个节点一条采集循环。
///
/// 顺序由结构固定：**先入账**，配额（M2）只能挂在它后面（C13）。
async fn run_service(config_dir: &std::path::Path) -> anyhow::Result<ExitCode> {
    use proxy_manager::agent::AgentClient;
    use proxy_manager::collect::{NodeCollector, SchedulePolicy};

    let (config, store) = open_store(config_dir).await?;
    let store = std::sync::Arc::new(store);
    // 单行设置在这里建出来：`quota::settings::apply` 只负责改，不负责建——
    // 那条 INSERT 分支必须凭空编出一个 display_timezone，而这个值只有配置知道。
    proxy_manager::quota::settings::ensure_initialized(
        &store,
        &config.server.quota.display_timezone,
    )
    .await?;
    let policy = SchedulePolicy::from_config(&config.server.collect);
    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);

    let mut tasks = Vec::new();
    for node in &config.nodes {
        let client =
            AgentClient::new(&node.node_id, &node.address, node.dns_name(), &node.materials_dir)?;
        let collector = NodeCollector::new(&node.node_id, client, policy.clone());
        tracing::info!(node_id = %node.node_id, interval = ?policy.target, "启动采集循环");
        tasks.push(tokio::spawn(collector.run(store.clone(), shutdown_rx.clone())));
    }
    if tasks.is_empty() {
        anyhow::bail!("nodes.toml 里没有可采集的节点");
    }

    // 失败开放的节点在启动日志里点名一次（C12）。配置里签过字不等于运行时没人再看见它。
    for node in &config.nodes {
        if let Some(reason) = node.fail_open_reason() {
            tracing::warn!(
                node_id = %node.node_id, reason = %reason,
                "该节点缺额度信息时放行：重启到下一次成功下发之间不受限"
            );
        }
    }

    // HTTP API 与控制台。绑回环；公网访问经本机反代（§5）。
    let nodes = std::sync::Arc::new(config.nodes.clone());
    let listener = tokio::net::TcpListener::bind(&config.server.listen.api).await?;
    let local = listener.local_addr()?;
    tracing::info!(listen = %local, "API 就绪");
    let api = tokio::spawn(proxy_manager::api::serve(
        store.clone(),
        nodes,
        listener,
        shutdown_rx.clone(),
    ));

    tokio::signal::ctrl_c().await?;
    tracing::info!("收到停止信号，等待在途采集结束");
    let _ = shutdown_tx.send(true);
    for task in tasks {
        let _ = task.await;
    }
    let _ = api.await;

    // 收尾时把四项判据打出来：一次运行结束时它们应当全为 0。
    let criteria = proxy_manager::collect::acceptance_criteria(&store).await?;
    tracing::info!(?criteria, all_zero = criteria.all_zero(), "四项判据");

    std::sync::Arc::try_unwrap(store).ok().unwrap().close().await;
    Ok(ExitCode::SUCCESS)
}

async fn open_store(config_dir: &std::path::Path) -> anyhow::Result<(Config, Store)> {
    let config = Config::load_dir(config_dir)?;
    let store = Store::open(&config.server.storage).await?;
    store.init_schema().await?;
    proxy_manager::ledger::runtime::sync_nodes(&store, &config).await?;
    Ok((config, store))
}

async fn runtime_command(command: RuntimeCommand) -> anyhow::Result<ExitCode> {
    use proxy_manager::ledger::runtime;
    use proxy_manager::ledger::settle::FirstSnapshot;

    match command {
        RuntimeCommand::List { config_dir } => {
            let (_, store) = open_store(&config_dir).await?;
            for window in runtime::list(&store).await? {
                println!(
                    "{}\t{}\t{}\t{}\tlast_sequence={}\t{}",
                    window.node_id,
                    window.runtime_id,
                    window.first_snapshot.as_str(),
                    window.approval_status,
                    window.last_sequence.map(|s| s.to_string()).unwrap_or_else(|| "-".into()),
                    window.window_state
                );
            }
            store.close().await;
            Ok(ExitCode::SUCCESS)
        }

        RuntimeCommand::Pending { config_dir } => {
            let (_, store) = open_store(&config_dir).await?;
            let pending = runtime::awaiting_approval(&store).await?;
            if pending.is_empty() {
                println!("没有待批准的 runtime。");
            }
            for (node_id, runtime_id, batches) in &pending {
                println!("{node_id}\t{runtime_id}\t收到 {batches} 份快照，尚未登记");
            }
            if !pending.is_empty() {
                println!();
                println!("未登记的 runtime 不可入账。批准前先确认它**不是**一次意外重启或误指端点");
                println!("产生的影子 runtime——那种 runtime 一旦批准，首快照会按策略吞掉已有累计");
                println!("或把历史流量一次性记成本周期用量。");
            }
            store.close().await;
            Ok(ExitCode::SUCCESS)
        }

        RuntimeCommand::Approve {
            config_dir,
            node,
            runtime: runtime_id,
            started_at_ms,
            policy,
            reason,
            actor,
        } => {
            let policy = FirstSnapshot::parse(&policy)
                .ok_or_else(|| anyhow::anyhow!("首快照策略必须是 baseline 或 include"))?;
            let (_, store) = open_store(&config_dir).await?;
            let window = runtime::approve(
                &store,
                &node,
                &runtime_id,
                started_at_ms,
                policy,
                &reason,
                &actor,
            )
            .await?;
            println!(
                "已批准 {} / {}：策略 {}，操作者 {}",
                window.node_id,
                window.runtime_id,
                window.first_snapshot.as_str(),
                actor
            );
            store.close().await;
            Ok(ExitCode::SUCCESS)
        }
    }
}

async fn ledger_command(command: LedgerCommand) -> anyhow::Result<ExitCode> {
    use proxy_manager::ledger::{settle, SettleOutcome};

    match command {
        LedgerCommand::Ingest { config_dir, node, file } => {
            let (config, store) = open_store(&config_dir).await?;
            let raw = std::fs::read(&file)?;
            let body = strip_http_head(&raw)?;
            let now = time::OffsetDateTime::now_utc();
            let receipt = settle::receive(
                &store,
                &node,
                &body,
                now,
                now,
                config.server.collect.max_clock_skew_secs,
            )
            .await?;
            let Some(receipt) = receipt else {
                println!("无法识别 envelope，只记安全日志，未落库。");
                store.close().await;
                return Ok(ExitCode::from(2));
            };
            println!(
                "回执：runtime_id={} sequence={} payload_sha256={} status={}",
                receipt.runtime_id, receipt.sequence, receipt.payload_sha256, receipt.status
            );

            let outcome = settle::settle_next(&store, &node, &receipt.runtime_id).await?;
            let code = match &outcome {
                SettleOutcome::Applied { sequence, ledger_rows, sequence_gap, .. } => {
                    println!("已入账：sequence={sequence} 账本行={ledger_rows}");
                    if let Some(gap) = sequence_gap {
                        println!(
                            "  跳号 {gap} 个序号：按累计差值结算，**不对缺失时段插值**（C26）"
                        );
                    }
                    ExitCode::SUCCESS
                }
                SettleOutcome::Rejected { reason, .. } => {
                    println!("整份拒绝：{reason}");
                    ExitCode::from(2)
                }
                SettleOutcome::Deferred { reason } => {
                    println!("暂不结算（批次保持 pending，可重试）：{reason}");
                    println!("  先跑 `proxy-manager runtime pending` 看待批准清单，批准后再 ingest 一次。");
                    ExitCode::from(3)
                }
                SettleOutcome::Superseded { sequence, .. } => {
                    println!("已被更新的批次取代：sequence={sequence}");
                    ExitCode::from(1)
                }
                SettleOutcome::Nothing => {
                    println!("没有待结算的批次。");
                    ExitCode::SUCCESS
                }
            };
            store.close().await;
            Ok(code)
        }

        LedgerCommand::Stats { config_dir } => {
            let (_, store) = open_store(&config_dir).await?;
            let criteria = proxy_manager::collect::acceptance_criteria(&store).await?;
            println!("四项判据（README §1 验收定义）：");
            println!("  负增量            {}", criteria.counter_regression);
            println!("  未知 runtime      {}", criteria.unknown_runtime);
            println!("  重复 sequence     {}", criteria.stale_sequence);
            println!("  unhealthy 入账    {}", criteria.unhealthy_accounted);
            println!();
            if criteria.all_zero() {
                println!("全为 0。");
                println!("注意这**不等于**「结算是对的」——它只说明这四类错误没发生。");
            } else {
                println!("**有非零项。** 这是 P0：四项判据里任何一项非零都意味着账本已经不可信。");
            }
            store.close().await;
            Ok(if criteria.all_zero() { ExitCode::SUCCESS } else { ExitCode::from(2) })
        }
    }
}

/// 允许直接喂完整往返记录：排查现场拿得到的往往是那个。
fn strip_http_head(raw: &[u8]) -> anyhow::Result<Vec<u8>> {
    if !raw.starts_with(b"HTTP/1.1 ") {
        return Ok(raw.to_vec());
    }
    let index =
        find_subsequence(raw, b"\r\n\r\n").ok_or_else(|| anyhow::anyhow!("响应缺少头体分隔"))?;
    let head = String::from_utf8_lossy(&raw[..index]).to_string();
    let body = &raw[index + 4..];
    if let Some(declared) = head
        .split("\r\n")
        .find_map(|line| line.strip_prefix("Content-Length: "))
        .and_then(|value| value.trim().parse::<usize>().ok())
    {
        anyhow::ensure!(
            declared == body.len(),
            "Content-Length 与实际长度不符：{declared} != {}",
            body.len()
        );
    }
    Ok(body.to_vec())
}

fn collect_command(command: CollectCommand) -> anyhow::Result<ExitCode> {
    match command {
        CollectCommand::CheckSnapshot { path } => {
            let raw = std::fs::read(&path)?;
            // 允许直接喂一份完整的 HTTP 往返记录：排查现场拿到的往往是那个。
            let body = match find_subsequence(&raw, b"\r\n\r\n") {
                Some(index) if raw.starts_with(b"HTTP/1.1 ") => {
                    let head = String::from_utf8_lossy(&raw[..index]).to_string();
                    let body = &raw[index + 4..];
                    if let Some(declared) = head
                        .split("\r\n")
                        .find_map(|line| line.strip_prefix("Content-Length: "))
                        .and_then(|value| value.trim().parse::<usize>().ok())
                    {
                        // 长度不符要先报：截断的 JSON 有可能仍然解析成功，
                        // 那会把一份残缺快照当成完整快照。
                        if declared != body.len() {
                            println!(
                                "拒绝：Content-Length 与实际长度不符：{declared} != {}",
                                body.len()
                            );
                            return Ok(ExitCode::from(2));
                        }
                    }
                    body.to_vec()
                }
                _ => raw,
            };

            match proxy_manager::collect::snapshot::parse(&body) {
                Ok(snapshot) => {
                    println!("接受：schema_version={}", snapshot.schema_version);
                    println!(
                        "  runtime_id 长度 {}，sequence {}",
                        snapshot.runtime_id.len(),
                        snapshot.sequence
                    );
                    println!(
                        "  health 可入账={} 审计缺口={}",
                        snapshot.health.billable(),
                        snapshot.health.audit_gap()
                    );
                    // 不打印身份名：它们是账号名。只报形状。
                    let identities: usize =
                        snapshot.inbounds.iter().map(|inbound| inbound.users.len()).sum();
                    println!("  inbounds {}，计费身份 {}", snapshot.inbounds.len(), identities);
                    if !snapshot.health.billable() {
                        println!("  注意：前三位有真值，这份快照**不可入账**（C4）");
                        return Ok(ExitCode::from(1));
                    }
                    Ok(ExitCode::SUCCESS)
                }
                Err(rejected) => {
                    // 「有用的失败」：说清楚是哪一条约束拒绝了它。
                    println!("拒绝：{rejected}");
                    Ok(ExitCode::from(2))
                }
            }
        }
    }
}

fn find_subsequence(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack.windows(needle.len()).position(|window| window == needle)
}

fn pki_command(command: PkiCommand) -> anyhow::Result<ExitCode> {
    match command {
        PkiCommand::Init { workspace } => {
            let workspace = Workspace::new(workspace);
            workspace.init(&issue::Validity::default())?;
            // 只打印公开信息：私钥路径可以说，私钥内容不行。
            println!("离线 workspace 已建立：{}", workspace.path().display());
            println!("root 私钥留在这里，**不要**复制到任何节点上。");
            Ok(ExitCode::SUCCESS)
        }

        PkiCommand::IssueNode { node, generation, dns, workspace, leaf_days } => {
            let workspace = Workspace::new(workspace);
            let validity = issue::Validity { leaf_days, ..Default::default() };
            let result = workspace.issue_node(&node, &generation, &dns, &validity)?;
            println!("节点 {} generation {} 已签发", result.node_id, result.generation);
            println!("  节点侧材料：{}", result.agent_dir.display());
            println!("  主控侧材料：{}", result.controller_dir.display());
            // 这个指纹是**带外核对**用的，写进 nodes.toml 的 agent_spki_sha256。
            println!("  server leaf SPKI：{}", result.server_leaf_spki);
            println!("  client leaf SPKI：{}", result.client_leaf_spki);
            println!("把节点侧材料带外送到节点上，并当面核对 server leaf 的指纹。");
            Ok(ExitCode::SUCCESS)
        }

        PkiCommand::Inspect { paths, warning_days, critical_days } => {
            if paths.is_empty() {
                anyhow::bail!("至少要给一个公证书路径");
            }
            for path in &paths {
                if path.extension().is_some_and(|ext| ext == "key") {
                    // 巡检没有任何理由去读私钥。这条拦在入口，
                    // 免得「为了让巡检能读」变成放宽私钥权限的借口。
                    anyhow::bail!("巡检只读公钥叶子，拒绝 {}", path.display());
                }
            }
            let thresholds = inspect::Thresholds { warning_days, critical_days };
            let now = time::OffsetDateTime::now_utc().unix_timestamp();
            let findings = inspect::inspect(&paths, thresholds, now);
            let mut rendered = String::new();
            for finding in &findings {
                rendered.push_str(&format!(
                    "{:?}\t{}\t{}\n",
                    finding.level,
                    finding.path.display(),
                    finding.reason
                ));
            }
            // 输出脱敏是硬要求，不是建议：巡检结果会进日志与告警，
            // 而告警最容易被转发到一个权限比它低的地方去。
            inspect::assert_output_is_safe(&rendered)?;
            print!("{rendered}");

            // C22：**退出码不是健康证据**。这个码只说明「这次巡检看到了什么」。
            Ok(match inspect::worst(&findings) {
                inspect::Level::Critical => ExitCode::from(2),
                inspect::Level::Warning => ExitCode::from(1),
                inspect::Level::Ok => ExitCode::SUCCESS,
            })
        }

        PkiCommand::Revoke { bundle, spki } => {
            let removed = pki::revoke_from_bundle(&bundle, &spki)?;
            println!("已从 {} 移除 {removed} 张 leaf。", bundle.display());
            println!("提醒：reload 顺序是**先节点后主控**——反过来会把自己关在门外。");
            Ok(ExitCode::SUCCESS)
        }
    }
}

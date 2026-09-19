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

    /// 用户：开通、改档、按名单对齐。
    ///
    /// 与控制台的 HTTP handler 调**同一套库函数**。做成 CLI 的理由很具体：
    /// SSO 要依赖对外域名、回调地址与出口 IP 这些主控仓库之外的事实，
    /// 任何一项没谈妥，登录就进不来。开通流程不该被那些事卡住。
    User {
        #[command(subcommand)]
        command: UserCommand,
    },

    /// SSO：按配置里的成员名单对齐库里的展示缓存。
    Sso {
        #[command(subcommand)]
        command: SsoCommand,
    },

    /// 出站目标审计。
    Audit {
        #[command(subcommand)]
        command: AuditCommand,
    },
}

async fn audit_command(command: AuditCommand) -> anyhow::Result<ExitCode> {
    let AuditCommand::Sync { config_dir } = command;
    let config = Config::load_dir(&config_dir)?;
    let Some(audit) = config.server.audit.as_ref() else {
        // 「没配」是一个确定的事实，说出来；而不是同步零台然后报告成功。
        println!("server.toml 里没有 [audit] 段，审计同步未启用。");
        return Ok(ExitCode::FAILURE);
    };
    let mut failed = false;
    for outcome in proxy_manager::audit::sync::sync_all(&config.nodes, audit).await {
        match outcome {
            Ok(report) => {
                println!(
                    "{}\t归档 +{}（{} 字节）\t活动 +{} 字节\t轮转 {}\t节点侧无法归类 {}",
                    report.node_id,
                    report.archives_fetched,
                    report.archive_bytes,
                    report.live_bytes,
                    report.rotations,
                    report.skipped_on_node
                );
                for line in report.anomalies.iter().chain(report.errors.iter()) {
                    println!("  ! {line}");
                    failed = true;
                }
            }
            Err(detail) => {
                println!("  ! {detail}");
                failed = true;
            }
        }
    }
    // 有未决项时退非零：这条命令会被放进排查脚本，而「打印了警告但退 0」
    // 在脚本里与「一切正常」不可区分。
    Ok(if failed { ExitCode::FAILURE } else { ExitCode::SUCCESS })
}

#[derive(Subcommand)]
enum AuditCommand {
    /// 立刻同步一轮，然后退出。服务里同一段代码按 `interval_secs` 周期跑。
    ///
    /// 手工跑一轮是安全的：同步只追加、不删除，偏移记在镜像目录里，
    /// 与服务并发跑最坏也只是两边各收一段、各自推进自己那份状态——
    /// 但那会让偏移打架，所以正常排查时先停服务或者只看不跑。
    Sync {
        #[arg(default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
}

#[derive(Subcommand)]
enum UserCommand {
    /// 列出已知用户，并标出**名单现在怎么判**。
    ///
    /// 两列可能不一致：库里那两列是上次登录时的快照，名单是当下的事实。
    /// 不一致不影响授权（那是每请求现解析的），影响的是控制台上显示的内容。
    List {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
    },
    /// 给一个用户开通身份：从 D11 的账号池里认领一个，落到全部启用配额的节点。
    ///
    /// **常规路径不需要它**——首次 SSO 登录会自动领取。这条命令是补救用的：
    /// 登录那次因为池空、节点未就绪或还没结算出槽位而没领到时，
    /// 处理完原因之后用它补上。
    ///
    /// **幂等**：已经有的直接返回，不再消耗一个名额。
    Grant {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        /// 登录名（泡游账号名，大小写敏感）。
        #[arg(long)]
        account: String,
        /// 操作者。必填并落进审计——这是一次影响他人的操作。
        #[arg(long)]
        actor: String,
    },
    /// 永久退役一个身份名：它在全部节点上的槽位一并置为 retired。
    ///
    /// **退役即永久**——复用一个槽位前必须更换 uPSK，而那要改节点配置并 reload。
    ///
    /// 两种用法：用过之后退役（归属保留，尾账仍算在原主头上），
    /// 或者把一个**从未认领**的名字永久挡在池子外面——部署侧的测试身份、
    /// 原型控制器的测试账号都属于后者：它们在节点的 inbound 里，
    /// 但凭据在别处流通，发给真人等于两个人共用一份。
    Retire {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        /// 身份名，例如 deploy-test。
        #[arg(long)]
        identity: String,
        #[arg(long)]
        actor: String,
        /// 为什么退役。落进审计。
        #[arg(long)]
        reason: String,
    },
    /// 显示某人的订阅地址。没有就发一条。
    ///
    /// token 是从 `(user_id, generation)` **派生**的，不是存起来的——
    /// 所以这条命令随时能再算一次同一个地址，而明文一个字节都没落过库。
    ///
    /// **输出里有凭据**：那个地址等于这个人的全部节点与密码。
    /// 不要贴进群、不要进工单（D10、§4.7）。
    Subscription {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        account: String,
    },
    /// 吊销某人的订阅，并**立刻发一条新的**。
    ///
    /// 吊销之后那个人所有已导入的客户端会在下一次更新时失败，
    /// 而且他不会收到任何通知——所以要 `--confirm`。
    /// 这是运维动作，不是用户自助（§4.7：自助轮换整个不做）。
    ReissueSubscription {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        account: String,
        #[arg(long)]
        actor: String,
        #[arg(long)]
        reason: String,
        #[arg(long, default_value_t = false)]
        confirm: bool,
    },
    /// 把一个用户换到另一档额度。
    ///
    /// 走 D21 的预览/确认纪律：**调低必须 `--confirm`**，调高不要求。
    Tier {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        account: String,
        #[arg(long, value_parser = ["normal", "advanced", "manage", "admin"])]
        group: String,
        #[arg(long)]
        actor: String,
        /// 看过预览之后再加它。不加时只打印预览，**不提交**。
        #[arg(long, default_value_t = false)]
        confirm: bool,
    },
}

#[derive(Subcommand)]
enum SsoCommand {
    /// 按名单对齐库里的 `role` 与 `quota_group`。
    ///
    /// 不加 `--apply` 只打印差异。角色直接同步（它只是展示缓存）；
    /// **档位走审计路径**，所以调低同样要 `--confirm`。
    Reconcile {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        #[arg(long)]
        actor: String,
        #[arg(long, default_value_t = false)]
        apply: bool,
        #[arg(long, default_value_t = false)]
        confirm: bool,
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

    /// 做一份**一致性备份**（R3），并当场核对它自洽。
    ///
    /// 用 `VACUUM INTO`：库是 WAL 模式，`cp` 拿到的是一个撕裂的快照，
    /// 而 `VACUUM INTO` 取一个读事务，与单写者并发是安全的，不阻塞写入。
    ///
    /// **备份完立刻核对。** 一份没被核对过的备份，与没有备份的区别只在
    /// 你自以为有——而这件事要到真的需要它的那天才会被发现。
    Backup {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        /// 备份文件路径。**已存在就拒绝**，不覆盖。
        #[arg(long)]
        out: PathBuf,
    },

    /// 核对任意一份库是否自洽——恢复演练的判据就是它。
    ///
    /// 主线只有一条：**总账必须能从账本重新算出来**。
    /// 「文件能打开」不是判据，那句话对一个被截断到一半的文件也成立。
    Verify {
        /// 要核对的库文件。只读打开，**不会改动它**。
        #[arg(long)]
        db: PathBuf,
    },

    /// 把旧的 `identity` 载荷重新按 zstd 编码。
    ///
    /// 压缩是从某一版才开始写的，之前的行原样留着（不做迁移，D22）。
    /// 这条命令把它们补上——**逐行核对往返**：解出来的字节必须与原文完全一致，
    /// 长度也必须对得上，任何一条对不上就整批回滚。
    Recompress {
        #[arg(long, default_value = "/etc/proxy-manager")]
        config_dir: PathBuf,
        /// 不加就是空跑：只报告会压多少、省多少，不写库。
        #[arg(long)]
        apply: bool,
        /// 每个事务处理多少行。默认 200——写入是单写者（D8），
        /// 一次吞太多会让正在采集的节点排在后面。
        #[arg(long, default_value_t = 200)]
        batch: i64,
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

        Command::User { command } => user_command(command).await,

        Command::Sso { command } => sso_command(command).await,
        Command::Audit { command } => audit_command(command).await,
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
        // 采集循环外面**总是**包一层下发。没开配额控制的节点在 `dispatch_round`
        // 的第一行就返回 `Disabled`——用一条运行时分支，而不是两种任务形态：
        // 两种形态意味着切换时要改的是启动代码，而不是一个配置字段。
        let service = proxy_manager::quota::service::QuotaService::from_config(
            collector,
            node,
            &config.server.quota,
            config.server.collect.snapshot_max_age_secs,
        );
        tracing::info!(
            node_id = %node.node_id,
            interval = ?policy.target,
            quota = node.quota_control.as_ref().is_some_and(|q| q.enabled),
            "启动采集与下发循环"
        );
        tasks.push(tokio::spawn(service.run(store.clone(), shutdown_rx.clone())));
    }
    if tasks.is_empty() {
        anyhow::bail!("nodes.toml 里没有可采集的节点");
    }

    // 出站目标审计的同步。整段 `[audit]` 缺省即不同步——那时 me-audit.html 取不到
    // 任何数据，所以要在启动日志里点名，而不是让人从「页面是空的」去反推。
    match &config.server.audit {
        Some(settings) => {
            tracing::info!(
                dir = %settings.dir.display(),
                interval_secs = settings.interval_secs,
                "启动审计同步循环：这个周期就是页面上数据的可见延迟"
            );
            tasks.push(tokio::spawn(proxy_manager::audit::sync::run(
                config.nodes.clone(),
                settings.clone(),
                shutdown_rx.clone(),
            )));
        }
        None => tracing::info!("server.toml 里没有 [audit] 段，不同步审计明细"),
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

    // 泡游 SSO。整段 `[sso]` 缺省即不启用——那时控制台只剩 break-glass 那条路，
    // 所以要在启动日志里点名，而不是让人从「登录按钮 404」去反推。
    let sso = match &config.server.sso {
        Some(settings) => {
            // 名单的事实来源是这个文件本身，每请求按 mtime + inode 重读；
            // 这里只是把路径记下来，并在启动时先读一次好点名。
            let sso = proxy_manager::sso::Sso::new_with_source(
                settings,
                &config_dir.join("server.toml"),
            )?;
            // 两把服务端密钥在这里就生成好：回调路径上第一次取 key 会写库，
            // 而那一步发生在校验 state **之前**（加固 3 要求「无任何写入地校验」）。
            proxy_manager::sso::warm_secrets(&store).await?;
            let roster = sso.roster()?;
            if roster.admin_count() == 0 {
                // 一个没有任何管理员的名单意味着**谁都改不了配置**，
                // 而它看起来和配好了完全一样。这是必须响的一条。
                tracing::error!("[sso].admins 是空的：登录进来的人全部是普通用户，无人可管理");
            }
            tracing::info!(
                admins = roster.admin_count(),
                callback = %settings.callback_url(),
                session_ttl_secs = settings.session_ttl_secs,
                "泡游 SSO 已启用"
            );
            Some(sso)
        }
        None => {
            tracing::warn!("没有配置 [sso]：登录入口不挂载，控制台只剩 break-glass");
            None
        }
    };

    // HTTP API 与控制台。绑回环；公网访问经本机反代（§5）。
    let nodes = std::sync::Arc::new(config.nodes.clone());
    let listener = tokio::net::TcpListener::bind(&config.server.listen.api).await?;
    let local = listener.local_addr()?;
    tracing::info!(listen = %local, "API 就绪");
    if let Some(settings) = &config.server.subscription {
        tracing::info!(
            entries = settings.entries.len(),
            base = %settings.public_base_url,
            "订阅已启用"
        );
    } else {
        tracing::warn!("没有配置 [subscription]：/sub/… 不挂载，用户拿不到客户端配置");
    }
    let api = tokio::spawn(proxy_manager::api::serve(
        store.clone(),
        nodes,
        sso,
        config.server.subscription.clone(),
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

        LedgerCommand::Backup { config_dir, out } => {
            anyhow::ensure!(!out.exists(), "{} 已存在：备份不覆盖既有文件", out.display());
            if let Some(parent) = out.parent() {
                std::fs::create_dir_all(parent)?;
            }
            // **只读配置，不走 `open_store`。** 那条路径会 `init_schema()` 加
            // `sync_nodes()`——两件都是写。备份工具不该在动手之前先改一遍被备份的对象。
            let config = Config::load_dir(&config_dir)?;
            let store = Store::open_for_backup(&config.server.storage.path).await?;
            store.assert_backup_preconditions().await?;
            // 先写 `.partial-`，核对通过才改名。中途失败时留下的是一个**名字就说明
            // 它没写完**的文件，而不是一份看起来正常、其实截断了的备份。
            // 这条是运维仓库里踩过才写下的纪律，照搬过来。
            let partial = out.with_extension("partial");
            let _ = std::fs::remove_file(&partial);
            // `VACUUM INTO` 取一个读事务：WAL 下与单写者并发安全，不阻塞采集入账。
            // 参数化绑定，不拼字符串——路径里的单引号会把语句拼坏。
            sqlx::query("VACUUM INTO ?")
                .bind(partial.to_string_lossy().as_ref())
                .execute(store.readers())
                .await?;
            store.close().await;
            // 只有属主读得到：库里有 `server_secrets`，一份备份就是一份密钥副本。
            // （`server_secrets` 进库进备份是**刻意**的——见 `schema/06_session.sql`：
            // 它与会话表同生共死，单独丢一个都会让所有会话失效。）
            #[cfg(unix)]
            std::fs::set_permissions(
                &partial,
                std::os::unix::fs::PermissionsExt::from_mode(0o600),
            )?;
            let bytes = std::fs::metadata(&partial)?.len();
            println!("已写出 {}（{} 字节）", partial.display(), bytes);
            println!();
            let code = print_verify(&partial).await?;
            if code != ExitCode::SUCCESS {
                println!();
                println!("**核对没过，不改名。** 那份写坏的东西留在 {}，", partial.display());
                println!("名字里就带着 partial——它不会被误当成一份可用的备份。");
                return Ok(code);
            }
            std::fs::rename(&partial, &out)?;
            println!();
            println!("核对通过，已改名为 {}", out.display());
            Ok(ExitCode::SUCCESS)
        }

        LedgerCommand::Verify { db } => print_verify(&db).await,

        LedgerCommand::Recompress { config_dir, apply, batch } => {
            anyhow::ensure!(batch > 0, "--batch 必须为正");
            let (_, store) = open_store(&config_dir).await?;
            let outcome = proxy_manager::ledger::payload::recompress(&store, apply, batch).await?;
            store.close().await;
            println!("identity 行：{}", outcome.candidates);
            println!("原文合计    ：{} 字节", outcome.raw_bytes);
            println!("压缩后合计  ：{} 字节", outcome.packed_bytes);
            if outcome.raw_bytes > 0 {
                println!(
                    "可省        ：{} 字节（{:.1}×）",
                    outcome.raw_bytes.saturating_sub(outcome.packed_bytes),
                    outcome.raw_bytes as f64 / outcome.packed_bytes.max(1) as f64
                );
            }
            if apply {
                println!("已改写      ：{} 行", outcome.rewritten);
                println!();
                println!("文件不会自己变小：SQLite 把腾出来的页留作空闲页。");
                println!("要把空间还给磁盘，停服务后跑一次 VACUUM。");
            } else {
                println!();
                println!("这是空跑，没有写库。确认之后加 --apply。");
            }
            Ok(ExitCode::SUCCESS)
        }
    }
}

/// 核对一份库并把结果打出来。退出码非 0 表示**这份库不自洽**。
async fn print_verify(db: &std::path::Path) -> anyhow::Result<ExitCode> {
    let store = Store::open_readonly(db).await?;
    // 备份产物旁边**不该有 -wal / -shm**：有的话说明它不是一个自足的文件，
    // 而「只拷主文件」正是那条会静默丢数据、却让 integrity_check 照样报 ok 的路。
    //
    // **只对看起来是备份产物的文件查这个。** 活库是 WAL，旁边本来就有这两个；
    // 对它报警是一条永远亮着的假警报，而假警报比没有警报更糟。
    // `VACUUM INTO` 出来的备份是 delete 模式，所以这个判据同时也认得出
    // 「有人直接 cp 了一个活库过来」——那份文件会是 wal 模式且带着孤儿 sidecar。
    let journal: String = sqlx::query("PRAGMA journal_mode")
        .fetch_one(store.readers())
        .await
        .map(|row| sqlx::Row::get::<String, _>(&row, 0))
        .unwrap_or_default();
    if !journal.eq_ignore_ascii_case("wal") {
        for sidecar in ["-wal", "-shm"] {
            let path = std::path::PathBuf::from(format!("{}{sidecar}", db.display()));
            if path.exists() {
                println!("  **旁边有 {sidecar}**    {}：这份文件不是自足的", path.display());
            }
        }
    } else {
        println!("  journal_mode      wal（这是一个活库，不是备份产物）");
    }
    let report = proxy_manager::store::verify::verify(&store).await?;
    store.close().await;

    println!("核对 {}", db.display());
    println!("  integrity_check   {}", report.integrity);
    println!("  外键违规          {}", report.foreign_key_violations);
    println!(
        "  表集合            {}",
        if report.missing_tables.is_empty() && report.unexpected_tables.is_empty() {
            "与 EXPECTED_TABLES 一致".to_string()
        } else {
            format!("缺 {:?}，多 {:?}", report.missing_tables, report.unexpected_tables)
        }
    );
    println!("  周期总账重算      {} 组", report.cycles_checked);
    println!("  永久累计重算      {} 个身份", report.lifetimes_checked);
    if report.epoch_high_water.is_empty() {
        // 这不是「干净」，是「一次重建会让四台节点全部 409 stale_epoch」。
        println!("  epoch 水位        **没有**（重建后需要人工补水位）");
    } else {
        for (node, runtime, epoch) in &report.epoch_high_water {
            println!("  epoch 水位        {node} / {} → {epoch}", &runtime[..8.min(runtime.len())]);
        }
    }
    for drift in &report.column_drift {
        println!("  **列与 DDL 不符**  {drift}");
    }
    for error in &report.shape_errors {
        println!("  **列的形状不对**  {error}");
    }
    for (codec, rows, packed, raw) in &report.payload_codecs {
        println!(
            "  载荷 {codec:<9}    {rows} 行，落库 {packed} 字节，原文 {raw} 字节{}",
            if *packed > 0 && raw > packed {
                format!("（{:.1}×）", *raw as f64 / *packed as f64)
            } else {
                String::new()
            }
        );
    }
    println!();
    if report.mismatches.is_empty() {
        println!("**总账与账本对得上。**");
    } else {
        println!("**对不上 {} 处：**", report.mismatches.len());
        for mismatch in &report.mismatches {
            println!("  {mismatch}");
        }
    }
    let interesting: Vec<_> =
        report.counts.iter().filter(|(_, n)| **n > 0).map(|(t, n)| format!("{t}={n}")).collect();
    println!();
    println!("行数：{}", interesting.join(" "));

    Ok(if report.ok() { ExitCode::SUCCESS } else { ExitCode::from(2) })
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

// ============ 用户与 SSO 子命令 ============

/// 名单（授权的事实来源）与库（展示缓存）都要，所以两个子命令都先开这一份。
async fn open_with_roster(
    config_dir: &std::path::Path,
) -> anyhow::Result<(proxy_manager::store::Store, proxy_manager::sso::roster::Roster)> {
    let (config, store) = open_store(config_dir).await?;
    let roster = match &config.server.sso {
        Some(settings) => proxy_manager::sso::roster::Roster::from_config(settings),
        // 没配 SSO 时是空名单：所有人都是普通用户。
        // **不是**「所有人都是管理员」——认证代码的失败模式是沉默地放行。
        None => proxy_manager::sso::roster::Roster::empty(),
    };
    Ok((store, roster))
}

async fn user_id_of(store: &proxy_manager::store::Store, account: &str) -> anyhow::Result<i64> {
    use sqlx::Row;
    let row = sqlx::query("SELECT user_id FROM users WHERE login_name = ?")
        .bind(account)
        .fetch_optional(store.readers())
        .await?;
    // 大小写敏感（§4.9）。找不到时把这一点说出来——
    // 「查无此人」和「大小写写错了」在命令行上长得一模一样。
    row.map(|row| row.get::<i64, _>(0)).ok_or_else(|| {
        anyhow::anyhow!("没有账号 {account:?}（登录名大小写敏感；本人登录过一次才会建号）")
    })
}

async fn user_command(command: UserCommand) -> anyhow::Result<ExitCode> {
    match command {
        UserCommand::List { config_dir } => {
            use sqlx::Row;
            let (store, roster) = open_with_roster(&config_dir).await?;
            let rows = sqlx::query(
                "SELECT login_name, display_name, role, quota_group, status, feishu_fs_id \
                 FROM users ORDER BY login_name",
            )
            .fetch_all(store.readers())
            .await?;
            if rows.is_empty() {
                println!("还没有任何用户：第一个人登录一次就会建号。");
                return Ok(ExitCode::SUCCESS);
            }
            println!(
                "{:<20} {:<16} {:<10} {:<10} {:<8} 名单判定",
                "登录名", "显示名", "角色", "档位", "状态"
            );
            let mut drift = 0usize;
            for row in rows {
                let login_name: String = row.get(0);
                let fs_id: Option<String> = row.get(5);
                let grade = roster.grade(&login_name, fs_id.as_deref());
                let role: String = row.get(2);
                let group: String = row.get(3);
                let same = role == grade.role.as_str() && group == grade.quota_group;
                if !same {
                    drift += 1;
                }
                println!(
                    "{:<20} {:<16} {:<10} {:<10} {:<8} {}",
                    login_name,
                    row.get::<String, _>(1),
                    role,
                    group,
                    row.get::<String, _>(4),
                    if same {
                        "一致".to_string()
                    } else {
                        format!("{}/{}", grade.role.as_str(), grade.quota_group)
                    }
                );
            }
            if drift > 0 {
                // 只是展示缓存对不上，**授权已经按名单在走了**。说清楚免得被当成越权。
                println!(
                    "\n{drift} 人的库内缓存与名单不一致。授权不受影响（每请求按名单现解析），\n                     受影响的只有控制台显示。跑 `proxy-manager sso reconcile` 对齐。"
                );
            }
            Ok(ExitCode::SUCCESS)
        }

        UserCommand::Grant { config_dir, account, actor } => {
            let (store, _) = open_with_roster(&config_dir).await?;
            let user_id = user_id_of(&store, &account).await?;
            let claim = proxy_manager::identity::claim_for_user(&store, user_id, &actor).await?;
            if claim.newly_claimed {
                println!("已开通：{account} → 身份 {}", claim.identity_name);
            } else {
                // 幂等不是「没生效」，说清楚免得有人重跑到池子见底。
                println!("{account} 早已开通：身份 {}（本次未消耗名额）", claim.identity_name);
            }
            println!("覆盖节点：{}", claim.nodes.join(", "));
            Ok(ExitCode::SUCCESS)
        }

        UserCommand::Retire { config_dir, identity, actor, reason } => {
            let (store, _) = open_with_roster(&config_dir).await?;
            let count = proxy_manager::identity::retire(&store, &identity, &actor, &reason).await?;
            println!("已退役 {identity}：{count} 个槽位（全部节点）。**不可复用。**");
            Ok(ExitCode::SUCCESS)
        }

        UserCommand::Subscription { config_dir, account } => {
            let (store, _) = open_with_roster(&config_dir).await?;
            let user_id = user_id_of(&store, &account).await?;
            let config = proxy_manager::config::Config::load_dir(&config_dir)?;
            let Some(settings) = &config.server.subscription else {
                anyhow::bail!("没有配置 [subscription]：订阅未启用");
            };
            let issued = proxy_manager::subscription::issue_for_user(&store, user_id).await?;
            let identity = proxy_manager::subscription::claimed_identity(&store, user_id).await?;
            println!("账号   : {account}");
            println!("身份   : {}", identity.as_deref().unwrap_or("(还没有分配)"));
            // **每一种到达方式都打出来。** 只打一条的话，拿到它的人会以为那就是
            // 全部——而在内网里拨公网地址是不通的，反过来也一样。
            for source in &settings.sources {
                println!(
                    "订阅 {:<12}: {}",
                    if source.note.is_empty() { &source.prefix } else { &source.note },
                    settings.subscription_url(&issued.plaintext, source)
                );
            }
            println!(
                "状态   : {}",
                if issued.newly_created { "本次新发" } else { "已有，重算出同一个" }
            );
            println!();
            println!("这个地址等于这个人的全部节点与密码。**不要贴进群、不要进工单。**");
            Ok(ExitCode::SUCCESS)
        }

        UserCommand::ReissueSubscription { config_dir, account, actor, reason, confirm } => {
            let (store, _) = open_with_roster(&config_dir).await?;
            let user_id = user_id_of(&store, &account).await?;
            if !confirm {
                println!("将吊销 {account} 现有的订阅并发一条新的。");
                println!("**他所有已导入的客户端会在下一次更新时失败，而且不会收到通知。**");
                println!("确认无误后加 --confirm。");
                return Ok(ExitCode::from(2));
            }
            let config = proxy_manager::config::Config::load_dir(&config_dir)?;
            let Some(settings) = &config.server.subscription else {
                anyhow::bail!("没有配置 [subscription]：订阅未启用");
            };
            let revoked = proxy_manager::subscription::revoke_for_user(&store, user_id).await?;
            let issued = proxy_manager::subscription::issue_for_user(&store, user_id).await?;
            tracing::info!(account = %account, actor = %actor, reason = %reason, revoked,
                           "订阅已重发");
            println!("已吊销 {revoked} 条，新地址：");
            for source in &settings.sources {
                println!(
                    "  {:<12} {}",
                    if source.note.is_empty() { &source.prefix } else { &source.note },
                    settings.subscription_url(&issued.plaintext, source)
                );
            }
            Ok(ExitCode::SUCCESS)
        }

        UserCommand::Tier { config_dir, account, group, actor, confirm } => {
            let (store, _) = open_with_roster(&config_dir).await?;
            user_id_of(&store, &account).await?;
            let scope = proxy_manager::quota::settings::Scope::UserGroup {
                login_name: account.clone(),
                new_group: group.clone(),
            };
            let cycle = proxy_manager::quota::cycle_key(time::OffsetDateTime::now_utc());
            let preview = proxy_manager::quota::settings::preview(&store, &scope, &cycle).await?;
            println!(
                "{account} → {group}：{} → {} 字节（{}）",
                preview.current_monthly_bytes,
                preview.new_monthly_bytes,
                if preview.is_reduction { "调低" } else { "调高" }
            );
            // 名单是对**这一刻的已结算用量**成立的，所以核验时刻必须打出来：
            // 后续流量仍可能增加用尽的人数。
            println!(
                "预计被闸断 {} 人（新增 {}，解出 {}），用量核验于 {}",
                preview.will_be_blocked.len(),
                preview.newly_blocked.len(),
                preview.newly_unblocked.len(),
                preview.usage_verified_at
            );
            if !confirm {
                println!("这是预览。确认无误后加 --confirm 提交。");
                return Ok(ExitCode::from(2));
            }
            let applied =
                proxy_manager::quota::settings::apply(&store, &scope, &actor, &cycle, true).await?;
            println!(
                "已提交：revision {}，待下发节点 {}",
                applied.revision,
                applied.nodes.join(", ")
            );
            Ok(ExitCode::SUCCESS)
        }
    }
}

async fn sso_command(command: SsoCommand) -> anyhow::Result<ExitCode> {
    match command {
        SsoCommand::Reconcile { config_dir, actor, apply, confirm } => {
            let (store, roster) = open_with_roster(&config_dir).await?;
            let plan = proxy_manager::sso::plan_reconcile(&store, &roster).await?;
            if plan.is_empty() {
                println!("库内缓存与名单一致，无需对齐。");
                return Ok(ExitCode::SUCCESS);
            }
            for rebind in &plan {
                if rebind.role_changed() {
                    println!(
                        "{}：角色 {} → {}",
                        rebind.login_name,
                        rebind.from_role,
                        rebind.to_role.as_str()
                    );
                }
                if rebind.group_changed() {
                    println!(
                        "{}：档位 {} → {}",
                        rebind.login_name, rebind.from_group, rebind.to_group
                    );
                }
            }
            if !apply {
                println!("\n这是预览。加 --apply 执行；其中调低档位还需要 --confirm。");
                return Ok(ExitCode::from(2));
            }

            let cycle = proxy_manager::quota::cycle_key(time::OffsetDateTime::now_utc());
            let mut blocked = 0usize;
            for rebind in &plan {
                if rebind.role_changed() {
                    // 角色只是展示缓存，直接同步。
                    proxy_manager::sso::sync_role(&store, rebind.user_id, rebind.to_role).await?;
                }
                if rebind.group_changed() {
                    let scope = proxy_manager::quota::settings::Scope::UserGroup {
                        login_name: rebind.login_name.clone(),
                        new_group: rebind.to_group.clone(),
                    };
                    // 改档位是一次影响他人的操作（D21）：走同一套预览/确认与审计。
                    match proxy_manager::quota::settings::apply(
                        &store, &scope, &actor, &cycle, confirm,
                    )
                    .await
                    {
                        Ok(applied) => println!(
                            "{}：档位已改，revision {}",
                            rebind.login_name, applied.revision
                        ),
                        Err(error) => {
                            // 调低未确认落在这里。**不中断整轮**：
                            // 其余人的对齐是好的，为一条没确认的调低把它们一起丢掉
                            // 只会让人再跑一遍、再看一遍同样的输出。
                            blocked += 1;
                            println!("{}：跳过（{error}）", rebind.login_name);
                        }
                    }
                }
            }
            if blocked > 0 {
                println!("\n{blocked} 人因需要确认而跳过：看过上面的预览后加 --confirm 重跑。");
                return Ok(ExitCode::from(2));
            }
            Ok(ExitCode::SUCCESS)
        }
    }
}

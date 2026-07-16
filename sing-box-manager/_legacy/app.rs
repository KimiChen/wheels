//! 运行态共享数据 + 配置热重载。
//! AppData 是 config+secrets 及其派生（token 表等）的不可变快照，放在 ArcSwap 里。
//! 编辑配置文件 → 校验通过则原子换入新快照；失败保留旧的。
//! 已生成用户的权限、配额和有效期变化无需重启 manager。

use crate::config::Config;
use crate::secrets::Secrets;
use anyhow::Result;
use arc_swap::ArcSwap;
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub struct AppData {
    pub cfg: Config,
    pub sec: Secrets,
    pub tokens: HashMap<String, String>, // 订阅 token -> 用户名
    pub method: String,                  // 入站方法（写进 ss://）
    pub host: String,                    // public_host
}

pub type Shared = Arc<ArcSwap<AppData>>;

/// 从磁盘加载 config+secrets，校验，构建快照。
pub fn load(config_path: &Path, secrets_path: &Path) -> Result<AppData> {
    let cfg = Config::load(config_path)?;
    cfg.validate()?; // 已校验 mode ∈ {ssm, reload}
    let users: Vec<String> = cfg.users.keys().cloned().collect();
    let exits = cfg.all_exits();
    let need_reality = cfg.singbox.inbound.kind == "vless-reality";
    let sec = Secrets::load_or_make(
        secrets_path,
        &cfg.terminal_nodes(),
        &users,
        &exits,
        need_reality,
    )?;

    let tokens = cfg
        .users
        .keys()
        .map(|name| (sec.user[name].token.clone(), name.clone()))
        .collect();
    let method = cfg
        .singbox
        .inbound
        .method
        .clone()
        .unwrap_or_else(|| cfg.singbox.relay_method.clone());
    let host = cfg.service.public_host.clone();
    Ok(AppData {
        cfg,
        sec,
        tokens,
        method,
        host,
    })
}

/// 监视配置文件，改动即校验并热换入。校验失败保留旧快照。
pub fn spawn_reload_watcher(
    shared: Shared,
    config_path: PathBuf,
    secrets_path: PathBuf,
) -> Result<()> {
    use notify::{RecursiveMode, Watcher};

    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<()>();
    let mut watcher = notify::recommended_watcher(move |res: notify::Result<notify::Event>| {
        if res.is_ok() {
            let _ = tx.send(());
        }
    })?;
    watcher.watch(&config_path, RecursiveMode::NonRecursive)?;

    tokio::spawn(async move {
        let _keep = watcher; // Watcher 必须存活
        while rx.recv().await.is_some() {
            while rx.try_recv().is_ok() {} // 合并同一次保存的多次事件
            match load(&config_path, &secrets_path) {
                Ok(d) => {
                    shared.store(Arc::new(d));
                    println!("[config reloaded]");
                }
                Err(e) => eprintln!("[config reload failed, keeping old] {e:#}"),
            }
        }
    });
    Ok(())
}

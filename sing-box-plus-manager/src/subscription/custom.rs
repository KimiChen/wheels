//! 把个人自定义节点渲染进订阅。
//!
//! # 三样东西进订阅，不是两样
//!
//! 1. 每条自定义上游 → 一个 `Custom-<名字>` 节点；
//! 2. 每条 SOCKS5 落地 → 一个 `Socks5-<名字>` 节点；
//! 3. **每条 SOCKS5 还带一个自动生成的 `select` 组**
//!    `Socks5-<名字>-先选前置节点`，而那个 SOCKS5 节点的 `dialer-proxy`
//!    指向这个组、不是直接指向某条上游。
//!
//! 第 3 条是旧站最值钱的设计：前置节点变成客户端里可点的一个选项，
//! 用户换前置不用回控制台改配置、不用重新导入订阅。少了它，
//! 「今天想从香港出、明天想直连」这件事每次都要重发一次订阅。
//!
//! # 兜底检查失败时**不发坏配置，也不静默丢**
//!
//! Clash 遇到重名或悬空引用会拒绝**整份**配置，用户看到的只有一句
//! 「订阅格式错误」。所以这里在渲染前自己查一遍：查不过就丢掉全部自定义
//! 节点、只发受管那部分，并在文件头写明丢了什么、为什么——
//! 与「有身份没凭据」时发一份明说的空配置是同一条口径。

use crate::custom::parse::{CustomProxy, CustomSocks5, Protocol};
use crate::custom::store::{
    custom_proxy_name, dialer_group_name, socks5_node_name, StoredProxy, StoredSocks5,
};
use crate::subscription::render::yaml_string;

/// 一个人的全部自定义节点。
#[derive(Debug, Clone, Copy)]
pub struct CustomNodes<'a> {
    pub proxies: &'a [StoredProxy],
    pub socks5: &'a [StoredSocks5],
}

impl CustomNodes<'_> {
    /// 没有自定义节点。受管订阅与用例走这一条。
    pub const NONE: CustomNodes<'static> = CustomNodes { proxies: &[], socks5: &[] };

    pub fn is_empty(&self) -> bool {
        self.proxies.is_empty() && self.socks5.is_empty()
    }

    /// 渲染后额外出现的**节点**名（不含拨号组）。要追加进每个现有代理组。
    pub fn node_names(&self) -> Vec<String> {
        self.proxies
            .iter()
            .map(|p| custom_proxy_name(&p.config.name))
            .chain(self.socks5.iter().map(|s| socks5_node_name(&s.config.name)))
            .collect()
    }

    /// 兜底检查。`Err` 里是**给人看的原因**，会原样写进订阅文件头。
    ///
    /// 应用层建/改时已经判过同样的事，这一道防的是绕过应用层的写入
    /// （直接写库、备份恢复、导入脚本）。
    pub fn check(&self, managed_nodes: &[&str], managed_groups: &[&str]) -> Result<(), String> {
        let mut seen: Vec<String> = Vec::new();
        let push = |name: String, seen: &mut Vec<String>| -> Result<(), String> {
            if managed_nodes.contains(&name.as_str()) || managed_groups.contains(&name.as_str()) {
                return Err(format!("「{name}」与受管入口或代理组重名"));
            }
            if seen.contains(&name) {
                return Err(format!("「{name}」重复出现"));
            }
            seen.push(name);
            Ok(())
        };
        for proxy in self.proxies {
            push(custom_proxy_name(&proxy.config.name), &mut seen)?;
        }
        for socks in self.socks5 {
            push(socks5_node_name(&socks.config.name), &mut seen)?;
            push(dialer_group_name(&socks.config.name), &mut seen)?;
        }
        // 每条 SOCKS5 的前置必须指向一个**渲染后真实存在**的名字。
        // 指不到就是悬空引用，而悬空引用让 Clash 拒绝整份配置。
        for socks in self.socks5 {
            let dialer = &socks.config.dialer_proxy;
            let ok = dialer == "DIRECT"
                || managed_nodes.contains(&dialer.as_str())
                || self.proxies.iter().any(|p| p.config.name == *dialer);
            if !ok {
                return Err(format!("SOCKS5「{}」的前置节点「{dialer}」不存在", socks.config.name));
            }
        }
        Ok(())
    }

    /// `proxies:` 段里追加的部分。调用方已经写完受管入口。
    pub fn render_proxies(&self, out: &mut String) {
        for proxy in self.proxies {
            render_proxy(out, &proxy.config);
        }
        for socks in self.socks5 {
            render_socks5(out, &socks.config);
        }
    }

    /// `proxy-groups:` 段末尾追加的拨号组。
    ///
    /// 组里的候选是「**这条 SOCKS5 当前选的那个** + 全部可用上游 + DIRECT」，
    /// 当前选的排在第一位——Clash 的 `select` 组默认选第一项，
    /// 所以这个顺序就是「用户在控制台选的那个默认生效」。
    pub fn render_dialer_groups(&self, out: &mut String, managed_nodes: &[&str]) {
        if self.socks5.is_empty() {
            return;
        }
        let mut upstreams: Vec<String> = managed_nodes.iter().map(|s| s.to_string()).collect();
        upstreams.extend(self.proxies.iter().map(|p| custom_proxy_name(&p.config.name)));
        for socks in self.socks5 {
            let selected = rendered_dialer(&socks.config.dialer_proxy, self.proxies);
            let mut options = vec![selected];
            for name in upstreams.iter().chain(std::iter::once(&"DIRECT".to_string())) {
                if !options.contains(name) {
                    options.push(name.clone());
                }
            }
            out.push_str(&format!(
                "  - name: {}\n",
                yaml_string(&dialer_group_name(&socks.config.name))
            ));
            out.push_str("    type: select\n    proxies:\n");
            for option in options {
                out.push_str(&format!("      - {}\n", yaml_string(&option)));
            }
        }
    }
}

/// 库里存的是原名，渲染后的名字带前缀。**两处口径必须一致**，
/// 否则 dialer 会指向一个渲染后并不存在的名字。
fn rendered_dialer(dialer: &str, proxies: &[StoredProxy]) -> String {
    if proxies.iter().any(|p| p.config.name == dialer) {
        custom_proxy_name(dialer)
    } else {
        dialer.to_string()
    }
}

fn render_proxy(out: &mut String, proxy: &CustomProxy) {
    out.push_str(&format!("  - name: {}\n", yaml_string(&custom_proxy_name(&proxy.name))));
    out.push_str(&format!("    type: {}\n", proxy.protocol.as_str()));
    out.push_str(&format!("    server: {}\n", yaml_string(&proxy.server)));
    out.push_str(&format!("    port: {}\n", proxy.port));
    out.push_str("    udp: true\n");
    if let Some(cipher) = &proxy.cipher {
        out.push_str(&format!("    cipher: {}\n", yaml_string(cipher)));
    }
    if let Some(password) = &proxy.password {
        out.push_str(&format!("    password: {}\n", yaml_string(password)));
    }
    if let Some(uuid) = &proxy.uuid {
        out.push_str(&format!("    uuid: {}\n", yaml_string(uuid)));
    }
    if proxy.protocol == Protocol::Vmess {
        // Clash 的键名是 `alterId`，**不是** kebab-case 的 `alter-id`。
        out.push_str(&format!("    alterId: {}\n", proxy.alter_id.unwrap_or(0)));
    }
    if let Some(flow) = &proxy.flow {
        out.push_str(&format!("    flow: {}\n", yaml_string(flow)));
    }
    if let Some(network) = &proxy.network {
        out.push_str(&format!("    network: {}\n", yaml_string(network)));
    }
    if let Some(tls) = proxy.tls {
        out.push_str(&format!("    tls: {tls}\n"));
    }
    if let Some(sni) = &proxy.sni {
        out.push_str(&format!("    sni: {}\n", yaml_string(sni)));
    }
    if let Some(servername) = &proxy.servername {
        out.push_str(&format!("    servername: {}\n", yaml_string(servername)));
    }
    if let Some(fingerprint) = &proxy.client_fingerprint {
        out.push_str(&format!("    client-fingerprint: {}\n", yaml_string(fingerprint)));
    }
    if let Some(alpn) = &proxy.alpn {
        out.push_str("    alpn:\n");
        for item in alpn {
            out.push_str(&format!("      - {}\n", yaml_string(item)));
        }
    }
    // **一律 false，而且一律写出来。** 不写的话客户端按自己的默认来，
    // 而有些客户端的默认是 true；分享链接是复制粘贴来的，
    // 让它能关掉证书校验等于把中间人位置送给任何能改那条链接的人。
    if proxy.tls.unwrap_or(false) || proxy.protocol == Protocol::Trojan {
        out.push_str("    skip-cert-verify: false\n");
    }
    if let Some(path) = &proxy.ws_path {
        out.push_str("    ws-opts:\n");
        out.push_str(&format!("      path: {}\n", yaml_string(path)));
        if let Some(host) = &proxy.ws_host {
            out.push_str("      headers:\n");
            out.push_str(&format!("        Host: {}\n", yaml_string(host)));
        }
    }
    if let Some(service) = &proxy.grpc_service_name {
        out.push_str("    grpc-opts:\n");
        out.push_str(&format!("      grpc-service-name: {}\n", yaml_string(service)));
    }
    if let Some(public_key) = &proxy.reality_public_key {
        out.push_str("    reality-opts:\n");
        out.push_str(&format!("      public-key: {}\n", yaml_string(public_key)));
        if let Some(short_id) = &proxy.reality_short_id {
            // 与受管入口同一条理由：十六进制不加引号会被 YAML 当成整数、
            // 前导零一并丢掉，而客户端对此只有一句「握手失败」。
            out.push_str(&format!("      short-id: {}\n", yaml_string(short_id)));
        }
        if let Some(spider) = &proxy.reality_spider_x {
            out.push_str(&format!("      spider-x: {}\n", yaml_string(spider)));
        }
    }
}

fn render_socks5(out: &mut String, socks: &CustomSocks5) {
    out.push_str(&format!("  - name: {}\n", yaml_string(&socks5_node_name(&socks.name))));
    out.push_str("    type: socks5\n");
    out.push_str(&format!("    server: {}\n", yaml_string(&socks.server)));
    out.push_str(&format!("    port: {}\n", socks.port));
    out.push_str("    udp: true\n");
    if !socks.username.is_empty() {
        out.push_str(&format!("    username: {}\n", yaml_string(&socks.username)));
        out.push_str(&format!("    password: {}\n", yaml_string(&socks.password)));
    }
    // 指向那个自动生成的组，**不是**直接指向上游——见模块文档。
    out.push_str(&format!("    dialer-proxy: {}\n", yaml_string(&dialer_group_name(&socks.name))));
}

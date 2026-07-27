//! 浏览器订阅落地页：UA/语言识别、设备导入深链与内联二维码。

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use percent_encoding::{utf8_percent_encode, NON_ALPHANUMERIC};
use qrcode::{render::svg, QrCode};

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub enum Language {
    Zh,
    En,
}

impl Language {
    fn text<'a>(self, zh: &'a str, en: &'a str) -> &'a str {
        match self {
            Self::Zh => zh,
            Self::En => en,
        }
    }

    fn code(self) -> &'static str {
        match self {
            Self::Zh => "zh-CN",
            Self::En => "en",
        }
    }
}

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
enum Platform {
    Ios,
    Android,
    Windows,
    Macos,
    Linux,
    Other,
}

impl Platform {
    const ALL: [Self; 6] = [
        Self::Ios,
        Self::Android,
        Self::Windows,
        Self::Macos,
        Self::Linux,
        Self::Other,
    ];

    fn id(self) -> &'static str {
        match self {
            Self::Ios => "ios",
            Self::Android => "android",
            Self::Windows => "windows",
            Self::Macos => "macos",
            Self::Linux => "linux",
            Self::Other => "other",
        }
    }

    fn label(self, lang: Language) -> &'static str {
        match self {
            Self::Ios => "iPhone / iPad",
            Self::Android => "Android",
            Self::Windows => "Windows",
            Self::Macos => "macOS",
            Self::Linux => "Linux",
            Self::Other => lang.text("其他设备", "Other"),
        }
    }
}

pub struct PageData<'a> {
    pub username: &'a str,
    pub subscription_url: &'a str,
    pub upload: u64,
    pub download: u64,
    pub quota: u64,
    pub expire: Option<i64>,
    pub reset: &'a str,
    pub period: &'a str,
    pub disabled: bool,
}

struct AppLink {
    name: &'static str,
    import_url: String,
    install_url: Option<&'static str>,
}

/// 浏览器返回落地页；命令行和代理客户端继续返回机器可读订阅。
pub fn wants_html(user_agent: &str, target: Option<&str>) -> bool {
    match target {
        Some("page") => return true,
        Some(_) => return false,
        None => {}
    }

    let ua = user_agent.to_ascii_lowercase();
    if ua.is_empty()
        || [
            "curl",
            "wget",
            "clash",
            "mihomo",
            "stash",
            "shadowrocket",
            "sing-box",
            "v2ray",
            "hiddify",
            "happ",
            "streisand",
            "surge",
            "loon",
            "quantumult",
        ]
        .iter()
        .any(|client| ua.contains(client))
    {
        return false;
    }

    ["mozilla/", "applewebkit/", "chrome/", "safari/", "firefox/"]
        .iter()
        .any(|browser| ua.contains(browser))
}

pub fn preferred_language(accept_language: &str, override_lang: Option<&str>) -> Language {
    match override_lang {
        Some("zh") | Some("zh-CN") => return Language::Zh,
        Some("en") => return Language::En,
        _ => {}
    }

    let mut best = ("en", -1.0_f32);
    for item in accept_language.split(',') {
        let mut parts = item.trim().split(';');
        let tag = parts.next().unwrap_or("").trim();
        if tag.is_empty() || tag == "*" {
            continue;
        }
        let quality = parts
            .find_map(|part| part.trim().strip_prefix("q="))
            .and_then(|q| q.parse::<f32>().ok())
            .unwrap_or(1.0);
        if quality > best.1 {
            best = (tag, quality);
        }
    }
    if best.0.to_ascii_lowercase().starts_with("zh") {
        Language::Zh
    } else {
        Language::En
    }
}

fn detect_platform(user_agent: &str) -> Platform {
    let ua = user_agent.to_ascii_lowercase();
    if ua.contains("iphone") || ua.contains("ipad") || ua.contains("ipod") {
        Platform::Ios
    } else if ua.contains("android") {
        Platform::Android
    } else if ua.contains("windows") {
        Platform::Windows
    } else if ua.contains("macintosh") || ua.contains("mac os x") {
        Platform::Macos
    } else if ua.contains("linux") {
        Platform::Linux
    } else {
        Platform::Other
    }
}

pub fn render_page(data: &PageData<'_>, user_agent: &str, language: Language) -> String {
    let platform = detect_platform(user_agent);
    let used = data.upload.saturating_add(data.download);
    let usage_percent = if data.quota == 0 {
        0.0
    } else {
        (used as f64 / data.quota as f64 * 100.0).clamp(0.0, 100.0)
    };
    let quota = if data.quota == 0 {
        language.text("不限量", "Unlimited").to_string()
    } else {
        format_bytes(data.quota)
    };
    let expire = data
        .expire
        .and_then(|timestamp| time::OffsetDateTime::from_unix_timestamp(timestamp).ok())
        .map(|value| value.date().to_string())
        .unwrap_or_else(|| language.text("永不过期", "Never").to_string());
    let reset = match data.reset {
        "monthly" => language.text("每月", "Monthly"),
        "yearly" => language.text("每年", "Yearly"),
        "never" => language.text("永不重置", "Never resets"),
        other => other,
    };
    let raw_url = with_target(data.subscription_url, "raw");
    let clash_url = with_target(data.subscription_url, "clash");
    let zh_url = format!("{}?target=page&lang=zh", data.subscription_url);
    let en_url = format!("{}?target=page&lang=en", data.subscription_url);
    let qr_svg = QrCode::new(data.subscription_url.as_bytes())
        .map(|code| {
            code.render()
                .min_dimensions(220, 220)
                .dark_color(svg::Color("#111827"))
                .light_color(svg::Color("#ffffff"))
                .build()
        })
        .unwrap_or_default();
    let status_text = if data.disabled {
        language.text("已停用", "Disabled")
    } else {
        language.text("可用", "Active")
    };
    let status_class = if data.disabled { "danger" } else { "active" };

    let mut platform_options = String::new();
    let mut app_sections = String::new();
    for item in Platform::ALL {
        platform_options.push_str(&format!(
            "<option value=\"{}\"{}>{}</option>",
            item.id(),
            if item == platform { " selected" } else { "" },
            escape_html(item.label(language))
        ));
        app_sections.push_str(&render_apps(item, platform, language, &raw_url, &clash_url));
    }

    let template = r#"<!doctype html>
<html lang="__LANG__">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="robots" content="noindex,nofollow,noarchive">
  <title>__PAGE_TITLE__</title>
  <style>
    :root{color-scheme:light dark;font-family:Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#eef2f7;color:#172033}
    *{box-sizing:border-box}body{margin:0;min-height:100vh;padding:28px 16px;background:linear-gradient(145deg,#e9eff8,#f8fafc)}
    main{width:min(680px,100%);margin:auto}.top{display:flex;align-items:center;justify-content:space-between;margin-bottom:16px}
    h1{font-size:22px;margin:0}.langs{display:flex;gap:6px}.langs a{color:#526078;text-decoration:none;padding:6px 9px;border-radius:9px}.langs a.current{background:#fff;color:#111827;box-shadow:0 2px 10px #20304a12}
    .card{background:#fff;border:1px solid #dfe6ef;border-radius:18px;padding:20px;box-shadow:0 12px 35px #21334d12;margin-bottom:14px}
    .identity{display:flex;align-items:center;justify-content:space-between;gap:12px}.user{font-size:20px;font-weight:700;overflow-wrap:anywhere}.badge{font-size:12px;font-weight:700;padding:6px 10px;border-radius:999px}.badge.active{background:#dcfce7;color:#15733a}.badge.danger{background:#fee2e2;color:#b42318}
    .usage{display:flex;justify-content:space-between;margin-top:20px;font-size:14px}.muted{color:#69758a}.bar{height:9px;background:#edf1f6;border-radius:99px;overflow:hidden;margin:9px 0 17px}.bar i{display:block;height:100%;width:__PERCENT__%;background:linear-gradient(90deg,#2563eb,#7c3aed);border-radius:inherit}
    .meta{display:grid;grid-template-columns:repeat(3,1fr);gap:9px}.meta div{background:#f7f9fc;border-radius:12px;padding:11px}.meta small{display:block;color:#748097;margin-bottom:5px}.meta strong{font-size:13px;overflow-wrap:anywhere}
    h2{font-size:16px;margin:0 0 13px}.url-row{display:flex;gap:8px}.url-row input{min-width:0;flex:1;border:1px solid #d8e0eb;background:#f8fafc;color:#263247;border-radius:11px;padding:11px}.button,.app-import{border:0;border-radius:11px;background:#2563eb;color:white;font-weight:700;padding:11px 15px;text-decoration:none;cursor:pointer;text-align:center}
    .device-head{display:flex;align-items:center;justify-content:space-between;gap:10px}.device-head select{border:1px solid #d8e0eb;background:#f8fafc;color:#263247;border-radius:10px;padding:8px 10px}.apps{display:grid;gap:10px;margin-top:12px}.app{display:grid;grid-template-columns:1fr auto;gap:4px 12px;align-items:center;border:1px solid #e1e7f0;border-radius:13px;padding:12px}.app strong{font-size:14px}.app small{color:#748097}.app-install{font-size:12px;color:#526078}.app-import{grid-row:1/3;grid-column:2;font-size:13px;padding:9px 12px}
    .qr{display:grid;place-items:center;text-align:center}.qr svg{width:min(240px,75vw);height:auto;background:#fff;padding:8px;border-radius:12px}.qr p{margin:10px 0 0}.cli{font-size:12px;word-break:break-all;background:#111827;color:#e5e7eb;padding:10px;border-radius:10px;margin-top:12px}
    footer{text-align:center;color:#8590a3;font-size:12px;padding:5px}@media(max-width:520px){body{padding:18px 12px}.card{padding:16px}.meta{grid-template-columns:1fr}.url-row{flex-direction:column}.device-head{align-items:flex-start;flex-direction:column}.device-head select{width:100%}}
    @media(prefers-color-scheme:dark){:root{background:#0c111b;color:#e8edf6}body{background:linear-gradient(145deg,#0a0f18,#111827)}.card{background:#151d2a;border-color:#273245}.langs a.current{background:#202b3b;color:#fff}.muted,.meta small,.app small{color:#9aa7bb}.meta div,.url-row input,.device-head select{background:#101722;color:#e8edf6;border-color:#2b374b}.bar{background:#293448}.app{border-color:#2b374b}.app-install{color:#b4bfd0}}
  </style>
</head>
<body>
<main>
  <div class="top"><h1>__PAGE_TITLE__</h1><nav class="langs"><a class="__ZH_CLASS__" href="__ZH_URL__">中文</a><a class="__EN_CLASS__" href="__EN_URL__">EN</a></nav></div>
  <section class="card">
    <div class="identity"><div><div class="muted">__ACCOUNT__</div><div class="user">__USERNAME__</div></div><span class="badge __STATUS_CLASS__">__STATUS__</span></div>
    <div class="usage"><span>__USED_LABEL__ <strong>__USED__</strong></span><span>__QUOTA_LABEL__ <strong>__QUOTA__</strong></span></div>
    <div class="bar"><i></i></div>
    <div class="meta"><div><small>__RESET_LABEL__</small><strong>__RESET__</strong></div><div><small>__PERIOD_LABEL__</small><strong>__PERIOD__</strong></div><div><small>__EXPIRE_LABEL__</small><strong>__EXPIRE__</strong></div></div>
  </section>
  <section class="card">
    <h2>__SUB_LINK__</h2><div class="url-row"><input id="sub-url" readonly value="__SUB_URL__"><button class="button" id="copy" data-copy="__COPY__" data-copied="__COPIED__">__COPY__</button></div>
    <div class="cli">curl -L '__SUB_URL__'</div>
  </section>
  <section class="card">
    <div class="device-head"><h2>__IMPORT_TITLE__</h2><select id="platform" aria-label="__DEVICE_LABEL__">__PLATFORM_OPTIONS__</select></div>
    __APP_SECTIONS__
  </section>
  <section class="card qr"><h2>__QR_TITLE__</h2>__QR_SVG__<p class="muted">__QR_HELP__</p></section>
  <footer>sing-box-manager</footer>
</main>
<script>
  const select=document.getElementById('platform');
  const switchPlatform=()=>document.querySelectorAll('[data-platform]').forEach(el=>el.hidden=el.dataset.platform!==select.value);
  select.addEventListener('change',switchPlatform);switchPlatform();
  const copyButton=document.getElementById('copy'),subUrl=document.getElementById('sub-url');
  copyButton.addEventListener('click',async()=>{try{await navigator.clipboard.writeText(subUrl.value)}catch(_){subUrl.select();document.execCommand('copy')}copyButton.textContent=copyButton.dataset.copied;setTimeout(()=>copyButton.textContent=copyButton.dataset.copy,1600)});
</script>
</body></html>"#;

    let replacements = [
        ("__LANG__", language.code().to_string()),
        (
            "__PAGE_TITLE__",
            language.text("我的订阅", "My Subscription").to_string(),
        ),
        (
            "__ZH_CLASS__",
            if language == Language::Zh {
                "current"
            } else {
                ""
            }
            .to_string(),
        ),
        (
            "__EN_CLASS__",
            if language == Language::En {
                "current"
            } else {
                ""
            }
            .to_string(),
        ),
        ("__ZH_URL__", escape_html(&zh_url)),
        ("__EN_URL__", escape_html(&en_url)),
        (
            "__ACCOUNT__",
            language
                .text("订阅账户", "Subscription account")
                .to_string(),
        ),
        ("__USERNAME__", escape_html(data.username)),
        ("__STATUS_CLASS__", status_class.to_string()),
        ("__STATUS__", status_text.to_string()),
        ("__USED_LABEL__", language.text("已用", "Used").to_string()),
        ("__USED__", format_bytes(used)),
        (
            "__QUOTA_LABEL__",
            language.text("总量", "Total").to_string(),
        ),
        ("__QUOTA__", quota),
        ("__PERCENT__", format!("{usage_percent:.2}")),
        (
            "__RESET_LABEL__",
            language.text("重置", "Reset").to_string(),
        ),
        ("__RESET__", escape_html(reset)),
        (
            "__PERIOD_LABEL__",
            language.text("当前周期", "Current period").to_string(),
        ),
        ("__PERIOD__", escape_html(data.period)),
        (
            "__EXPIRE_LABEL__",
            language.text("有效期", "Expires").to_string(),
        ),
        ("__EXPIRE__", escape_html(&expire)),
        (
            "__SUB_LINK__",
            language.text("订阅地址", "Subscription URL").to_string(),
        ),
        ("__SUB_URL__", escape_html(data.subscription_url)),
        ("__COPY__", language.text("复制", "Copy").to_string()),
        ("__COPIED__", language.text("已复制", "Copied").to_string()),
        (
            "__IMPORT_TITLE__",
            language.text("导入客户端", "Import to app").to_string(),
        ),
        (
            "__DEVICE_LABEL__",
            language.text("设备", "Device").to_string(),
        ),
        ("__PLATFORM_OPTIONS__", platform_options),
        ("__APP_SECTIONS__", app_sections),
        (
            "__QR_TITLE__",
            language.text("扫码导入", "Scan to import").to_string(),
        ),
        ("__QR_SVG__", qr_svg),
        (
            "__QR_HELP__",
            language
                .text(
                    "使用代理客户端扫描此二维码",
                    "Scan this QR code in your proxy app",
                )
                .to_string(),
        ),
    ];
    replacements
        .into_iter()
        .fold(template.to_string(), |page, (key, value)| {
            page.replace(key, &value)
        })
}

fn render_apps(
    platform: Platform,
    selected: Platform,
    language: Language,
    raw_url: &str,
    clash_url: &str,
) -> String {
    let encoded_raw = encode_url(raw_url);
    let encoded_clash = encode_url(clash_url);
    let apps = match platform {
        Platform::Ios => vec![
            AppLink {
                name: "Shadowrocket",
                import_url: format!("sub://{}", STANDARD.encode(raw_url)),
                install_url: Some("https://apps.apple.com/app/shadowrocket/id932747118"),
            },
            AppLink {
                name: "Stash",
                import_url: format!("stash://install-config?url={encoded_clash}"),
                install_url: Some("https://apps.apple.com/app/stash-rule-based-proxy/id1596063349"),
            },
        ],
        Platform::Android => vec![
            AppLink {
                name: "v2rayNG",
                import_url: format!(
                    "v2rayng://install-config?name=sing-box-manager&url={encoded_raw}"
                ),
                install_url: Some("https://github.com/2dust/v2rayNG/releases/latest"),
            },
            AppLink {
                name: "Clash Meta",
                import_url: format!(
                    "clashmeta://install-config?name=sing-box-manager&url={encoded_clash}"
                ),
                install_url: Some(
                    "https://github.com/MetaCubeX/ClashMetaForAndroid/releases/latest",
                ),
            },
        ],
        Platform::Windows | Platform::Macos | Platform::Linux => vec![AppLink {
            name: "Clash Verge Rev",
            import_url: format!("clash://install-config?url={encoded_clash}"),
            install_url: Some("https://github.com/clash-verge-rev/clash-verge-rev/releases/latest"),
        }],
        Platform::Other => vec![AppLink {
            name: language.text("通用订阅", "Raw subscription"),
            import_url: raw_url.to_string(),
            install_url: None,
        }],
    };

    let cards = apps
        .into_iter()
        .map(|app| {
            let install = app
                .install_url
                .map(|url| {
                    format!(
                        "<a class=\"app-install\" href=\"{}\" target=\"_blank\" rel=\"noreferrer\">{}</a>",
                        escape_html(url),
                        language.text("获取客户端", "Get app")
                    )
                })
                .unwrap_or_default();
            format!(
                "<article class=\"app\"><strong>{}</strong><small>{}</small>{}<a class=\"app-import\" href=\"{}\">{}</a></article>",
                escape_html(app.name),
                language.text("支持一键导入订阅", "One-click subscription import"),
                install,
                escape_html(&app.import_url),
                language.text("导入", "Import")
            )
        })
        .collect::<String>();
    format!(
        "<div class=\"apps\" data-platform=\"{}\"{}>{cards}</div>",
        platform.id(),
        if platform == selected { "" } else { " hidden" }
    )
}

fn with_target(url: &str, target: &str) -> String {
    format!("{url}?target={target}")
}

fn encode_url(value: &str) -> String {
    utf8_percent_encode(value, NON_ALPHANUMERIC).to_string()
}

fn format_bytes(bytes: u64) -> String {
    if bytes < 1024 {
        return format!("{bytes} B");
    }
    let units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    let mut value = bytes as f64;
    let mut unit = 0;
    while value >= 1024.0 && unit < units.len() - 1 {
        value /= 1024.0;
        unit += 1;
    }
    format!("{value:.1} {}", units[unit - 1])
}

fn escape_html(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&#39;")
}

#[cfg(test)]
mod tests {
    use super::{
        detect_platform, preferred_language, render_page, wants_html, Language, PageData, Platform,
    };

    #[test]
    fn distinguishes_browsers_from_clients_and_curl() {
        assert!(wants_html("Mozilla/5.0 Chrome/138 Safari/537.36", None));
        assert!(!wants_html("curl/8.7.1", None));
        assert!(!wants_html("clash.meta v1.19.0", None));
        assert!(!wants_html("Mozilla/5.0 Chrome/138", Some("raw")));
        assert!(wants_html("curl/8.7.1", Some("page")));
    }

    #[test]
    fn selects_language_by_quality_and_allows_override() {
        assert_eq!(
            preferred_language("zh-CN,zh;q=0.9,en;q=0.8", None),
            Language::Zh
        );
        assert_eq!(preferred_language("zh;q=0.5,en;q=1", None), Language::En);
        assert_eq!(preferred_language("en-US", Some("zh")), Language::Zh);
    }

    #[test]
    fn detects_common_platforms() {
        assert_eq!(detect_platform("Mozilla/5.0 (iPhone)"), Platform::Ios);
        assert_eq!(
            detect_platform("Mozilla/5.0 (Linux; Android 15)"),
            Platform::Android
        );
        assert_eq!(
            detect_platform("Mozilla/5.0 (Windows NT 10.0)"),
            Platform::Windows
        );
    }

    #[test]
    fn page_is_localized_escaped_and_contains_qr_and_import_links() {
        let page = render_page(
            &PageData {
                username: "<alice>",
                subscription_url: "https://sub.example.com/sub/token",
                upload: 1024,
                download: 2048,
                quota: 10240,
                expire: None,
                reset: "monthly",
                period: "2027-03",
                disabled: false,
            },
            "Mozilla/5.0 (iPhone)",
            Language::Zh,
        );
        assert!(page.contains("lang=\"zh-CN\""));
        assert!(page.contains("&lt;alice&gt;"));
        assert!(!page.contains("<alice>"));
        assert!(page.contains("<svg"));
        assert!(page.contains("sub://"));
        assert!(page.contains("stash://install-config"));
    }
}

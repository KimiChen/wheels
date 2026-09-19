//! 前端资源的机械门禁（README §4.11、C3）。
//!
//! 这一组用例**不起服务、不连库**，只对嵌进二进制的字符串做断言。
//! 它们守的都是「看起来正常、实际已经错了」那一类问题：
//! u64 被浮点削掉高位、CSP 把内联脚本挡了、kit 换版本而这边没跟。

use proxy_manager::web;

/// C3 在前端的机械化：`api.js` 里**不得出现任何把字节转成浮点的调用**。
///
/// 为什么用文本断言而不是评审：`Number("18446744073709551615")` 不报错，
/// 它安静地给出 18446744073709552000。这种错在小数字上永远看不出来，
/// 只在某个大流量用户身上出现一次，而且没有任何异常可查。
///
/// 允许 `Number.isFinite`（它是判定，不是转换），所以匹配的是带左括号的调用形式。
#[test]
fn front_end_never_converts_bytes_to_float() {
    let source = web::asset("assets/api.js").expect("api.js 必须嵌入").0;
    for forbidden in ["Number(", "parseInt(", "parseFloat(", "JSON.parse(", "toFixed("] {
        assert!(
            !source.contains(forbidden),
            "api.js 里出现了 `{forbidden}`：u64 走它会被静默削掉高位（C3）"
        );
    }
    // 反向锁：确实是用 BigInt 在算，而不是干脆没做格式化。
    assert!(source.contains("BigInt("), "api.js 必须用 BigInt 解析字节");
}

/// **每一个登录态页面都要能退出登录。**
///
/// 服务端 `POST /api/v1/auth/logout` 一直是实现好的（`WriteSubject` 的双提交 CSRF、
/// 成功后清两个 cookie），而十个页面的退出按钮一直是一句
/// `data-toast="原型页面不执行退出。"`——**点一下弹个提示，会话原封不动**。
/// 共用设备上的人会以为自己退了。
///
/// 这条同时钉住两端：页面上有按钮，脚本里有那次 POST。少任何一端，
/// 那个按钮就退回成一个「看起来能点」的装饰。
#[test]
fn every_authenticated_page_can_log_out() {
    for name in proxy_manager::web::page_names() {
        if proxy_manager::web::is_public(name) {
            continue; // 登录页与越权提示页没有会话可退
        }
        let body = proxy_manager::web::page_body(name).expect("页面必须嵌进二进制");
        if name == "index.html" {
            continue; // 兼容跳转页，没有外壳
        }
        assert!(body.contains("data-pm-logout"), "{name} 没有可用的退出按钮");
        assert!(!body.contains("原型页面不执行退出"), "{name} 还留着那个只弹提示的原型退出按钮");
    }

    let (script, _) = proxy_manager::web::asset("assets/api.js").expect("api.js 必须嵌进二进制");
    assert!(script.contains("/auth/logout"), "api.js 里没有那次 POST");
    assert!(script.contains("x-csrf-token"), "退出是写操作，必须带双提交 CSRF 头，否则服务端会拒");
    // 名字要与服务端一致：写错一个字母，退出按钮会变成「点了没反应」。
    //
    // **连引号一起比。** 只 `contains(CSRF_COOKIE)` 挡不住把名字写成
    // `__Host-pm_csrf_x`——那个串把正确的名字整个包在里面，断言照样通过。
    let quoted = format!("\"{}\"", proxy_manager::api::session::CSRF_COOKIE);
    assert!(
        script.contains(&quoted),
        "api.js 读的 CSRF cookie 名与服务端的 {} 对不上",
        proxy_manager::api::session::CSRF_COOKIE
    );
}

/// 复制按钮必须**先确认自己跑在真实模式下**，不能只判「这串字符像不像 URL」。
///
/// 原型页面里那一格是脱敏占位符 `https://<控制台域名>/sub/Proxy-••••.yaml`，
/// 而它同样以 `https://` 开头。第一版的判据是 `text.startsWith("http")`，
/// 于是点一下就把一串圆点复制走了——复制成功的提示照常弹出，
/// 而那串东西看起来完全像个地址，直到粘进客户端才发现不对。
///
/// **这个 bug 是在浏览器里真跑一遍才发现的**：读代码时那个判断看着是对的。
/// 所以这里用文本锁住，与 `front_end_never_converts_bytes_to_float` 同一条纪律。
#[test]
fn copy_button_requires_live_mode() {
    let (script, _) = proxy_manager::web::asset("assets/api.js").expect("api.js 必须嵌进二进制");
    assert!(
        script.contains("dataset.pmMode !== \"live\""),
        "复制处理器不再判 live 模式：原型页面上的脱敏占位符会被当成地址复制走"
    );
    // 上面那条的前提：页面里确实有一个以 https:// 开头的脱敏占位符。
    // 占位符哪天换成别的写法，这条断言先红，提醒人重看那个判据。
    let html = proxy_manager::web::page_body("me.html").expect("me.html 必须嵌进二进制");
    assert!(
        html.contains('\u{2022}'),
        "me.html 里的示例地址不再用掩码字符：请重新确认复制按钮的判据还成立"
    );
}

/// CSP 是 `script-src 'self'`（§4.11），所以页面里**不能有内联脚本体**。
///
/// 内联脚本被 CSP 挡掉时页面不会报错到用户面前，只是那段逻辑静默不执行。
#[test]
fn pages_have_no_inline_script_bodies() {
    for name in web::page_names() {
        let body = web::page_body(name).expect("页面必须嵌入");
        let mut rest = body;
        while let Some(at) = rest.find("<script") {
            let tail = &rest[at..];
            let close = tail.find('>').expect("script 标签未闭合");
            let open_tag = &tail[..=close];
            let after = &tail[close + 1..];
            let end = after.find("</script>").expect("script 未配对");
            assert!(
                after[..end].trim().is_empty(),
                "{name} 里有内联脚本体，CSP `script-src 'self'` 会把它挡掉：{open_tag}"
            );
            rest = &after[end..];
        }
    }
}

/// 认证页面必须带 `stamp()` 的锚点，否则角色下发不到页面上。
///
/// 缺锚点时 `serve_page` 会退回原文——页面仍然能打开，但导航会按
/// localStorage 里的原型偏好走。**这正是那种不报错的退化**，所以要锁住。
#[test]
fn authenticated_pages_carry_the_role_anchor() {
    for name in web::page_names() {
        if web::is_public(name) {
            continue;
        }
        let body = web::page_body(name).expect("页面必须嵌入");
        assert!(
            body.contains(web::BODY_ANCHOR),
            "{name} 缺少 `{}` 锚点，服务端盖不上角色",
            web::BODY_ANCHOR
        );
    }
}

/// 登录页必须带 `stamp_sso()` 的锚点。
///
/// 与上面那条同一类：缺锚点时 `stamp_sso` 退回原文，页面照样打得开，
/// 但「有没有配 SSO」就再也显示不出来了——**不报错的退化**。
#[test]
fn login_page_carries_the_sso_anchor() {
    let body = web::page_body("login.html").expect("登录页必须嵌入");
    assert!(body.contains(web::LOGIN_ANCHOR), "缺少 `{}` 锚点", web::LOGIN_ANCHOR);
    let stamped = web::stamp_sso(body, true);
    assert!(stamped.contains(r#"data-pm-sso="on""#));
    assert!(stamped.contains(r#"class="pm-login""#), "盖属性不得把 class 顶掉");
}

/// 页面上的 `data-roles` 与服务端的 `PAGES` 可见性表必须一致。
///
/// 两份表职责不同（一份拦截、一份让界面讲得通），但**不能互相矛盾**：
/// 服务端放行而导航里没有入口，用户会以为功能没上；
/// 反过来则是点进去就 403，比没有入口更糟。
#[test]
fn nav_visibility_matches_server_side_table() {
    use proxy_manager::api::session::Role;
    // 任取一页的侧栏即可：12 页共用同一份导航结构。
    let nav = web::page_body("overview.html").unwrap();
    for name in web::page_names() {
        if web::is_public(name) || name == "index.html" {
            continue;
        }
        let Some(at) = nav.find(&format!("href=\"{name}\"")) else {
            continue; // 该页不在侧栏里（例如 unauthorized）
        };
        // 往后找本条链接的 data-roles。
        let tail = &nav[at..];
        let close = tail.find('>').unwrap();
        let roles =
            tail[..close].split("data-roles=\"").nth(1).map(|s| s.split('"').next().unwrap());
        let Some(roles) = roles else { continue };
        let nav_admin_only = !roles.split_whitespace().any(|r| r == "user");
        let server_admin_only = !web::visible_to(name, Some(Role::User));
        assert_eq!(
            nav_admin_only, server_admin_only,
            "{name} 的导航可见性（data-roles=\"{roles}\"）与服务端拦截表不一致"
        );
    }
}

/// `web-standard-kit` 是同仓库的另一个子项目，这里**按摘要锁定**。
///
/// 与 M0 锁上游节点样例是同一条纪律（`docs/integration-contract.md` §2.3）：
/// 被依赖的东西可以变，但**不能静默地变**。kit 改了样式或脚本 API，
/// 这里先红，人再决定是跟还是钉住——而不是某天发现控制台的对话框不弹了。
///
/// 锁红的处置：确认 kit 的改动无害后更新下面的摘要，并在提交信息里写清楚跟的是哪一版。
#[test]
fn kit_is_digest_locked() {
    // (路径, sha256, 字节数)。字节数是给人看的——摘要变了但大小几乎没变，
    // 通常意味着改的是内容而不是新增功能。
    const LOCKED: &[(&str, &str, usize)] = &[
        (
            "web-standard-kit/style.css",
            "48781d1d6fa6c41101319f71cfe60ed7c118fc74b9be9e15753b6d89316cfeaa",
            86620,
        ),
        (
            "web-standard-kit/script.js",
            "47e39907b283cbd862fee703aa147cd9e947efd6cb4c7db2acd3507b68e2beb0",
            27775,
        ),
    ];

    let mut seen = 0;
    for (path, body) in web::kit_files() {
        let (_, expected, size) = LOCKED
            .iter()
            .find(|(key, _, _)| *key == path)
            .unwrap_or_else(|| panic!("{path} 是新嵌入的 kit 文件，但没有对应的摘要锁"));
        assert_eq!(body.len(), *size, "{path} 大小变了");
        assert_eq!(&sha256_hex(body.as_bytes()), expected, "{path} 内容变了");
        seen += 1;
    }
    assert_eq!(seen, LOCKED.len(), "有锁定的 kit 文件没被嵌入");
}

fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    hasher.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

/// vendored 第三方库按摘要锁定，与 kit 同一条纪律。
///
/// 对第三方代码这条比对 kit 更重要：kit 是同仓库的、改动看得见，
/// 而 `vendor/` 里的东西是从公网取回来的。锁住摘要意味着
/// **任何一次「顺手升个版本」都必须是一次显式的、留痕的决定**。
/// 锁红时的处置写在 `web/vendor/README.md`：重新跨源核对、扫一遍动态求值与网络调用、
/// 确认许可证没变，然后更新摘要并在提交信息里写清楚跟的是哪一版。
#[test]
fn vendor_is_digest_locked() {
    const LOCKED: &[(&str, &str, usize)] = &[
        (
            "vendor/uplot.iife.min.js",
            "19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f",
            51081,
        ),
        (
            "vendor/uplot.min.css",
            "df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04",
            1857,
        ),
        (
            "vendor/uplot.LICENSE",
            "8f989229699b4fe2f1a0432d0e9edc338a8a911e250e2d1b01ecd770a5f5b1bd",
            1078,
        ),
    ];

    let mut seen = 0;
    for (path, body) in web::vendor_files() {
        let (_, expected, size) = LOCKED
            .iter()
            .find(|(key, _, _)| *key == path)
            .unwrap_or_else(|| panic!("{path} 是新 vendored 的文件，但没有对应的摘要锁"));
        assert_eq!(body.len(), *size, "{path} 大小变了");
        assert_eq!(&sha256_hex(body.as_bytes()), expected, "{path} 内容变了");
        seen += 1;
    }
    assert_eq!(seen, LOCKED.len(), "有锁定的 vendored 文件没被嵌入");
}

/// 许可证必须**和库一起发**，不是只躺在仓库里。
///
/// MIT 要求分发时附带许可证。库嵌进了二进制、许可证没嵌，
/// 等于分发了一份不带许可证的副本。
#[test]
fn vendored_license_ships_with_the_binary() {
    let (license, _) = web::asset("vendor/uplot.LICENSE").expect("许可证必须随二进制发布");
    assert!(license.contains("MIT License"), "许可证内容不对");
    assert!(license.contains("Leon Sorokin"), "版权行不能丢");
}

/// **整个前端只允许一处把字节转成浮点**，就在 `chart.js` 的 `toPixelScale`。
///
/// canvas 的坐标是浮点，这一步几何上绕不开；不可接受的是让这个值流回文案里。
/// 所以规则不是「不许转」，而是「只许转一次，且必须叫这个名字」——
/// 名字让评审一眼看出它的用途，计数让第二处转换加不进来。
#[test]
fn chart_has_exactly_one_float_conversion() {
    let source = web::asset("assets/chart.js").expect("chart.js 必须嵌入").0;
    assert_eq!(
        source.matches("Number(").count(),
        1,
        "chart.js 里的浮点转换不止一处：多出来的那处很可能流进了 tooltip"
    );
    // 缩进随 IIFE 包裹变化，所以按去空白后的形状比，而不是逐字比。
    let squashed: String = source.chars().filter(|c| !c.is_whitespace()).collect();
    assert!(
        squashed.contains("functiontoPixelScale(decimalText){returnNumber(decimalText);}"),
        "唯一那处转换必须是 `toPixelScale`，改名或改写都要重新想清楚它会流到哪里"
    );
    for forbidden in ["parseInt(", "parseFloat(", "toFixed("] {
        assert!(!source.contains(forbidden), "chart.js 里出现了 `{forbidden}`");
    }
    // tooltip 必须回查服务端返回的原始字符串，而不是把画图用的浮点当精确值。
    assert!(source.contains("exactText"), "tooltip 必须从 exact[] 回查");
}

/// 我们自己的脚本**一个全局都不许留**。
///
/// 三个脚本（kit、`app.js`、`api.js`，趋势页再加 `chart.js`）都是 classic script，
/// 共用同一个全局词法作用域。同名 `const` 会直接抛 SyntaxError——整个文件一行不执行；
/// 同名 `function` 更糟，它**不报错**，只是后加载的那个把全局绑定换掉，
/// 于是 kit 内部调自己的函数时拿到的是我们的同名实现。
/// 这两种都在本轮真的发生过，而且页面上都看不出异常。
#[test]
fn our_scripts_leak_no_globals() {
    for name in ["assets/app.js", "assets/api.js", "assets/chart.js"] {
        let source = web::asset(name).unwrap_or_else(|| panic!("{name} 必须嵌入")).0;
        let leaked: Vec<&str> = source
            .lines()
            .filter(|line| {
                ["const ", "let ", "var ", "function ", "class ", "async function "]
                    .iter()
                    .any(|kw| line.starts_with(kw))
            })
            .collect();
        assert!(leaked.is_empty(), "{name} 在顶层留下了全局声明：{leaked:?}");
        assert!(source.contains("(function () {"), "{name} 必须整体包在 IIFE 里");
    }
    // 唯一的出口是这个显式命名空间。
    let api = web::asset("assets/api.js").unwrap().0;
    assert!(api.contains("window.pm = {"), "api.js 必须显式导出 window.pm");
}

/// vendored 文件必须**真的被 git 跟踪**。
///
/// 仓库根的 `.gitignore` 里有一条 Go 约定的 `vendor/`，它正好命中
/// `sing-box-plus-manager/web/vendor/`。文件在本机存在、`cargo build` 正常，
/// 但别人克隆下来第一次构建就会因为 `include_str!` 找不到文件而失败——
/// **本机全绿、别人编不过**，是最难靠自测发现的一类问题。
/// 这次是靠人眼看 `git status` 发现的，不该靠人眼。
#[test]
fn vendored_files_are_actually_committed() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
    for (path, _) in web::vendor_files() {
        let relative = format!("web/{path}");
        let output = std::process::Command::new("git")
            .arg("-C")
            .arg(root)
            .arg("check-ignore")
            .arg("-q")
            .arg(root.join(&relative))
            .status()
            .expect("应当能运行 git");
        // check-ignore 退出码：0 = 被忽略，1 = 没被忽略。
        assert!(
            !output.success(),
            "{relative} 被 .gitignore 忽略了：它是 include_str! 的输入，\
             不进仓库的话别人克隆下来编译不过"
        );
    }
}

/// **页面里不许出现编出来的具体值。**
///
/// 这一条是补上的，起因是一次不完整的体检：上一轮我清了 me.html、me-audit.html
/// 与几处外壳，然后在文档里写了「整站体检」——而 overview 的「今日结算 3.80 TiB」、
/// users 的「出站目标 · kimi 访问 www.example.com 1,284 次」、nodes 的
/// 「节点详情 · hk-01」、settings 的飞书 app_id 都还在生产上显示着。
///
/// 靠人记得去看每一页是不行的。这里列的是**曾经真的出现在生产页面上**的那些串：
/// 编出来的节点名、用户名、域名、数字。它们要么接真实数据，要么进
/// `data-pm-prototype`（live 模式会把整块换成一句说明），要么删掉。
///
/// 加新页面时这条会跟着生效；要放行一个串，先想清楚它凭什么不是假数据。
#[test]
fn pages_carry_no_invented_values() {
    // 每一条都带上它当初出现在哪里，免得将来有人不知道为什么禁它。
    const FORBIDDEN: &[(&str, &str)] = &[
        ("hk-01", "编出来的节点名，真实节点叫 paoyou-work-*"),
        ("us-01", "同上"),
        ("jp-01", "同上"),
        ("sg-01", "同上，而且这个节点从来没存在过"),
        ("fr-01", "同上，曾出现在一条写死的 P1 假告警里"),
        ("la-01", "同上"),
        ("www.example.com", "伪造的个人访问记录"),
        ("api.example.org", "同上"),
        ("cdn.example.net", "同上"),
        ("203.0.113.7", "同上"),
        ("wangwei", "编出来的用户名"),
        ("u-kimi", "编出来的身份名"),
        ("pool-0042", "同上"),
        ("cli_", "飞书 app_id 的前缀，D27 之后这套凭据不存在了"),
        ("892 / 1024", "编出来的身份池水位"),
        ("3.80 TiB", "编出来的当日结算量"),
        ("4,182,996,341,208", "同上，精确到个位，对账时会被直接引用"),
    ];

    for name in proxy_manager::web::page_names() {
        let body = proxy_manager::web::page_body(name).expect("页面必须嵌进二进制");
        // 两处豁免，各有理由：
        //  * HTML 注释——不渲染，而解释「这里原来是什么」正需要提到那些串；
        //  * `[data-pm-prototype]` 的子树——live 模式下 app.js 把整块换成一句说明，
        //    里面的内容一个字都不会显示，而那正是「这一块还没有数据源」的正式标记。
        let visible = strip_prototype_blocks(&strip_html_comments(body));
        for (needle, why) in FORBIDDEN {
            assert!(
                !visible.contains(needle),
                "{name} 里还有编出来的 {needle:?}（{why}）。\
                 要么接真实数据，要么放进 data-pm-prototype，要么删掉"
            );
        }
    }
}

/// 去掉每一个带 `data-pm-prototype` 的元素连同它的子树。
///
/// 按标签名做深度匹配。**这个函数自己有一条用例**（`stripper_actually_strips`）——
/// 一个什么都不剥的剥离器会让上面那条断言全盘放行，而它看起来仍然是绿的。
fn strip_prototype_blocks(body: &str) -> String {
    let mut out = String::new();
    let mut rest = body;
    while let Some(pos) = rest.find("data-pm-prototype") {
        let Some(open) = rest[..pos].rfind('<') else { break };
        let tag: String =
            rest[open + 1..].chars().take_while(|c| c.is_ascii_alphanumeric()).collect();
        if tag.is_empty() {
            break;
        }
        out.push_str(&rest[..open]);
        let Some(gt) = rest[open..].find('>') else { break };
        let open_pat = format!("<{tag}");
        let close_pat = format!("</{tag}>");
        let mut cursor = open + gt + 1;
        let mut depth = 1usize;
        loop {
            let next_open = rest[cursor..].find(&open_pat).map(|i| cursor + i);
            let Some(next_close) = rest[cursor..].find(&close_pat).map(|i| cursor + i) else {
                return out; // 没有闭合标签：剩下的整段都当作被标记的内容丢掉
            };
            match next_open {
                Some(o) if o < next_close => {
                    depth += 1;
                    cursor = o + open_pat.len();
                }
                _ => {
                    depth -= 1;
                    cursor = next_close + close_pat.len();
                    if depth == 0 {
                        break;
                    }
                }
            }
        }
        rest = &rest[cursor..];
    }
    out.push_str(rest);
    out
}

/// **剥离器必须真的在剥。**
///
/// 没有这条，上面那条禁令会在剥离器坏掉的那天静默变成一句空话——
/// 它会把整页都当成「被标记的内容」放行，而测试照常是绿的。
#[test]
fn stripper_actually_strips() {
    let html = "<p>保留</p><div data-pm-prototype=\"x\"><div>内层</div>假数据</div><p>也保留</p>";
    let out = strip_prototype_blocks(html);
    assert!(out.contains("保留") && out.contains("也保留"), "剥多了：{out}");
    assert!(!out.contains("假数据") && !out.contains("内层"), "剥少了：{out}");

    // 没有标记时一个字都不该动。
    let plain = "<p>什么都没有</p>";
    assert_eq!(strip_prototype_blocks(plain), plain);

    // 真实页面上剥完之后必须**还剩下东西**——剥成空串等于全盘放行。
    let body = proxy_manager::web::page_body("overview.html").unwrap();
    let stripped = strip_prototype_blocks(&strip_html_comments(body));
    assert!(
        stripped.len() > body.len() / 2,
        "overview.html 被剥掉了一大半（{} -> {}），多半是深度匹配写错了",
        body.len(),
        stripped.len()
    );
}

/// 去掉 HTML 注释。注释不会渲染，而解释「这里原来是什么」正需要提到那些串。
fn strip_html_comments(body: &str) -> String {
    let mut out = String::with_capacity(body.len());
    let mut rest = body;
    while let Some(open) = rest.find("<!--") {
        out.push_str(&rest[..open]);
        match rest[open..].find("-->") {
            Some(close) => rest = &rest[open + close + 3..],
            None => return out,
        }
    }
    out.push_str(rest);
    out
}

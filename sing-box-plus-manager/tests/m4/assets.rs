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

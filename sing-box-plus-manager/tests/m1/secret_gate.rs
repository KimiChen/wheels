//! 密钥不进版本库——**门禁，不是嘱咐**（`docs/threat-model.md` §2.5）。
//!
//! > 同类系统的 PKI 文档明确写着「该目录及其备份不得进入 Git」，
//! > 实际却有 118 个文件被跟踪，包括离线 root CA 私钥与发布签名私钥，
//! > 且没有任何忽略规则覆盖它们。
//!
//! 所以这一组用例**真的去 git 里查**。一句写在文档里的嘱咐挡不住任何东西。

use std::process::Command;

fn repo_root() -> std::path::PathBuf {
    std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

fn git(args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo_root())
        .args(args)
        .output()
        .expect("运行 git 失败：密钥门禁无法核对");
    assert!(
        output.status.success(),
        "git {args:?} 失败：{}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).to_string()
}

/// 仓库里现在没有任何被跟踪的密钥材料。
#[test]
fn 没有任何密钥材料被git跟踪() {
    let tracked = git(&["ls-files"]);
    let offenders: Vec<&str> = tracked
        .lines()
        .filter(|path| {
            // 只看密钥材料与 PKI **产物目录**。
            // 不能写成 `contains("/pki/")`——那会把 src/pki/ 这样的源码目录也算进来，
            // 于是门禁变成一条恒红的断言，最后被人改宽或删掉。
            path.ends_with(".key")
                || path.ends_with(".pem")
                || path.ends_with(".p12")
                || path.ends_with(".pfx")
                || path.contains("pki-workspace/")
                || path.starts_with("pki/")
                || path.starts_with("ca/")
        })
        .collect();
    assert!(
        offenders.is_empty(),
        "以下文件被 git 跟踪，可能是密钥材料：{offenders:?}。\
         同类系统正是在这里栽过——README 写明「密钥不得进 Git」，实际 CA 私钥已被跟踪。"
    );
}

/// 忽略规则真的覆盖 PKI workspace，而不是只覆盖了几个后缀。
///
/// 用 `git check-ignore` 而不是读 `.gitignore` 文本：
/// 前者问的是「git 实际会不会忽略它」，后者只是「文件里写了这么一行」。
#[test]
fn 忽略规则真的覆盖pki产物() {
    let candidates = [
        "sing-box-plus-manager/pki-workspace/root/root-ca.key",
        "sing-box-plus-manager/pki-workspace/root/root-ca.pem",
        // 非 PEM 的产物也必须被盖住：serials.txt 泄露的是签发历史，
        // 而 *.key / *.pem 那两条规则管不到它。
        "sing-box-plus-manager/pki-workspace/serials.txt",
        "sing-box-plus-manager/pki/anything",
        "sing-box-plus-manager/ca/anything",
        "sing-box-plus-manager/config/server.toml",
        "sing-box-plus-manager/config/nodes.toml",
        "sing-box-plus-manager/some.key",
        "sing-box-plus-manager/some.pem",
    ];
    let mut missed = Vec::new();
    for candidate in candidates {
        let output = Command::new("git")
            .arg("-C")
            .arg(repo_root().parent().unwrap())
            .args(["check-ignore", "-q", candidate])
            .status()
            .expect("运行 git check-ignore 失败");
        if !output.success() {
            missed.push(candidate);
        }
    }
    assert!(
        missed.is_empty(),
        "以下路径**不会**被 git 忽略：{missed:?}。\
         建 PKI 的第一步是写门禁，第二步才是生成密钥；顺序反了就会重演同一件事。"
    );
}

/// 反向证据：门禁不是「凡是路径都忽略」。
///
/// 没有这一条，上面那条对一个 `.gitignore` 里写了 `*` 的仓库同样是绿的。
#[test]
fn 忽略规则没有宽到把源码也盖住() {
    for tracked in [
        "sing-box-plus-manager/src/pki/mod.rs",
        "sing-box-plus-manager/README.md",
        "sing-box-plus-manager/config/server.example.toml",
    ] {
        let status = Command::new("git")
            .arg("-C")
            .arg(repo_root().parent().unwrap())
            .args(["check-ignore", "-q", tracked])
            .status()
            .expect("运行 git check-ignore 失败");
        assert!(!status.success(), "{tracked} 不该被忽略");
    }
}

//! 审计同步器：轮转、偏移与「说不清就停住」。
//!
//! 这些分支在真节点上极难复现——轮转按 16 MiB 触发，实测要 1.4 天到 63 天一次，
//! 而「文件变短但没有新归档」根本没有已知的制造方法。所以 `Source` 抽成了 trait：
//! 这里用一个假节点把每一种时序都摆出来。

use std::collections::BTreeMap;
use std::sync::Mutex;

use proxy_manager::audit::sync::{sync_node, NodeMirror, Source};
use proxy_manager::error::Result;
use proxy_manager_wire::audit;
use proxy_manager_wire::command::{AuditFileEntry, AuditListing};

const STAMP_A: &str = "20260919T041530.123456789Z";
const STAMP_B: &str = "20260919T051530.123456789Z";
const IDENTITY: &str = "u_example_01";

/// 一个假节点。行为要与真 agent 一致，否则用例证明不了任何东西：
/// 活动文件**截到最后一个完整行**，偏移超界返回空而不是报错。
#[derive(Default)]
struct FakeNode {
    files: Mutex<BTreeMap<String, Vec<u8>>>,
    skipped: Mutex<usize>,
    fail_reads: Mutex<Vec<String>>,
}

impl FakeNode {
    fn put(&self, name: &str, body: &[u8]) {
        self.files.lock().unwrap().insert(name.to_string(), body.to_vec());
    }

    fn append(&self, name: &str, body: &[u8]) {
        self.files.lock().unwrap().entry(name.to_string()).or_default().extend_from_slice(body);
    }

    /// 轮转：活动文件改名成归档，同名文件变回空。
    fn rotate(&self, active: &str, stamp: &str) {
        let mut files = self.files.lock().unwrap();
        let body = files.insert(active.to_string(), Vec::new()).unwrap_or_default();
        files.insert(format!("{active}.{stamp}"), body);
    }
}

impl Source for FakeNode {
    async fn list(&self) -> Result<AuditListing> {
        let files = self
            .files
            .lock()
            .unwrap()
            .iter()
            .map(|(name, body)| AuditFileEntry {
                name: name.clone(),
                size: body.len() as u64,
                active: audit::name_kind(name).map(|(k, _)| k) == Some(audit::NameKind::Active),
            })
            .collect();
        Ok(AuditListing { files, skipped: *self.skipped.lock().unwrap() })
    }

    async fn read(&self, file: &str, offset: u64) -> Result<Vec<u8>> {
        if self.fail_reads.lock().unwrap().iter().any(|f| f == file) {
            return Err(proxy_manager::error::Error::Agent("假装读失败".into()));
        }
        let files = self.files.lock().unwrap();
        let body = files
            .get(file)
            .ok_or_else(|| proxy_manager::error::Error::Agent("没有这个文件".into()))?;
        if offset as usize >= body.len() {
            return Ok(Vec::new());
        }
        let tail = &body[offset as usize..];
        let is_active = audit::name_kind(file).map(|(k, _)| k) == Some(audit::NameKind::Active);
        Ok(if is_active { audit::whole_lines(tail).to_vec() } else { tail.to_vec() })
    }
}

struct Fixture {
    node: FakeNode,
    mirror: NodeMirror,
    _dir: tempfile::TempDir,
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().unwrap();
    let mirror = NodeMirror::new(dir.path(), "node-example-01").unwrap();
    Fixture { node: FakeNode::default(), mirror, _dir: dir }
}

impl Fixture {
    async fn sync(&self) -> proxy_manager::audit::sync::Report {
        sync_node("node-example-01", &self.node, &self.mirror).await.unwrap()
    }

    fn local(&self, name: &str) -> Vec<u8> {
        std::fs::read(self.mirror.dir().join(name)).unwrap_or_default()
    }

    fn local_names(&self) -> Vec<String> {
        let mut names: Vec<String> = std::fs::read_dir(self.mirror.dir())
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().to_string())
            .filter(|n| audit::name_kind(n).is_some())
            .collect();
        names.sort();
        names
    }
}

fn active() -> String {
    audit::active_file_name(IDENTITY)
}

#[tokio::test]
async fn 活动文件按偏移增量收下不重复不遗漏() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\n");

    let first = f.sync().await;
    assert_eq!(first.live_bytes, 4);
    assert_eq!(f.local(&a), b"one\n");

    f.node.append(&a, b"two\n");
    let second = f.sync().await;
    // 只收新增的那一段，不是整份重拉。
    assert_eq!(second.live_bytes, 4, "只该收下新增的 4 字节");
    assert_eq!(f.local(&a), b"one\ntwo\n", "本机是完整的两行，没有重复");

    // 没有新增时不该再收任何东西。这条抓的是「每轮全量重拉」——
    // 那种实现看起来完全正常，只是白白搬运。
    let third = f.sync().await;
    assert_eq!(third.live_bytes, 0);
    assert_eq!(f.local(&a), b"one\ntwo\n");
}

#[tokio::test]
async fn 半行留在节点上不会被收下也不会挡住后续() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\nhal");
    assert_eq!(f.sync().await.live_bytes, 4, "半行不收");
    assert_eq!(f.local(&a), b"one\n");

    f.node.append(&a, b"f\n");
    assert_eq!(f.sync().await.live_bytes, 5);
    assert_eq!(f.local(&a), b"one\nhalf\n", "补完之后整行才进来");
}

#[tokio::test]
async fn 轮转之后本机既有归档也不重复计算() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\ntwo\n");
    f.sync().await;
    assert_eq!(f.local(&a), b"one\ntwo\n");

    // 节点又写了一行才轮转——那一行我们还没收到，它会随归档一起来。
    f.node.append(&a, b"three\n");
    f.node.rotate(&a, STAMP_A);
    f.node.append(&a, b"four\n");

    let report = f.sync().await;
    assert_eq!(report.rotations, 1);
    assert_eq!(report.archives_fetched, 1);
    assert!(report.clean(), "{report:?}");

    let archive = format!("{a}.{STAMP_A}");
    assert_eq!(f.local(&archive), b"one\ntwo\nthree\n", "归档是完整的三行");
    // live 副本被清空后重新累积——**归档已经落到本机才清**，所以这不是删除，是去重。
    assert_eq!(f.local(&a), b"four\n");

    // 全部本机内容合起来，每一行恰好一次。
    let all = [f.local(&archive), f.local(&a)].concat();
    assert_eq!(all, b"one\ntwo\nthree\nfour\n");
}

/// **这条是这个模块存在的理由。**
///
/// 「文件比我记的偏移还短」不是可靠的轮转信号：同步是周期性的，新文件很可能
/// 已经长过旧偏移。用那个信号的话，这里会从 offset=8 处接着读一个新文件，
/// 静默跳过它的前 8 字节——不报错、不计数、不可见。
#[tokio::test]
async fn 新文件已经长过旧偏移时仍然认得出轮转() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\ntwo\n"); // 8 字节
    f.sync().await;

    // 轮转，且新文件立刻长到比 8 字节更长。
    f.node.rotate(&a, STAMP_A);
    f.node.append(&a, b"aaaa\nbbbb\ncccc\n"); // 15 字节 > 8

    let report = f.sync().await;
    assert_eq!(report.rotations, 1, "必须认出这是一次轮转");
    assert_eq!(f.local(&a), b"aaaa\nbbbb\ncccc\n", "新文件要从头收，一个字节都不能跳");
    assert_eq!(f.local(&format!("{a}.{STAMP_A}")), b"one\ntwo\n");
}

#[tokio::test]
async fn 一轮之内发生多次轮转也不丢() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\n");
    f.sync().await;

    f.node.append(&a, b"two\n");
    f.node.rotate(&a, STAMP_A);
    f.node.append(&a, b"three\n");
    f.node.rotate(&a, STAMP_B);
    f.node.append(&a, b"four\n");

    let report = f.sync().await;
    assert_eq!(report.archives_fetched, 2);
    let all = [f.local(&format!("{a}.{STAMP_A}")), f.local(&format!("{a}.{STAMP_B}")), f.local(&a)]
        .concat();
    assert_eq!(all, b"one\ntwo\nthree\nfour\n", "四行各一次，顺序也对");
}

/// 变短却没有更新的归档——说不清。不猜：停住这个文件并报出来。
/// 而且**停是粘的**：文件会长回去，条件会自己消失，那时数据已经漏了。
#[tokio::test]
async fn 变短但没有新归档时停住而且停是粘的() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\ntwo\nthree\n");
    f.sync().await;

    // 文件凭空变短，没有任何归档出现。
    f.node.put(&a, b"x\n");
    let first = f.sync().await;
    assert_eq!(first.anomalies.len(), 1, "{first:?}");
    assert_eq!(first.live_bytes, 0, "说不清的时候一个字节都不收");
    assert_eq!(f.local(&a), b"one\ntwo\nthree\n", "本机已有的不动");

    // 文件长回去，`size < offset` 这个条件自己消失了。没有粘性的话
    // 这一轮会从 offset=14 处接着读，把新文件的前 14 字节静默漏掉。
    f.node.put(&a, b"x\ny\nz\naaaaaaaaaaaaaaaaaaaa\n");
    let second = f.sync().await;
    assert_eq!(second.anomalies.len(), 1, "仍然停着：{second:?}");
    assert_eq!(second.live_bytes, 0);

    // 一份新归档才解得开。
    f.node.rotate(&a, STAMP_A);
    f.node.append(&a, b"fresh\n");
    let third = f.sync().await;
    assert!(third.anomalies.is_empty(), "新归档应当解开停住状态：{third:?}");
    assert_eq!(third.rotations, 1);
    assert_eq!(f.local(&a), b"fresh\n");
}

#[tokio::test]
async fn 已经拉过的归档不会重复拉() {
    let f = fixture();
    let a = active();
    f.node.put(&format!("{a}.{STAMP_A}"), b"archived\n");
    let first = f.sync().await;
    assert_eq!(first.archives_fetched, 1);

    let second = f.sync().await;
    assert_eq!(second.archives_fetched, 0, "归档不再变化，拉过一次就够");
    assert_eq!(second.archive_bytes, 0);
}

#[tokio::test]
async fn 大小对不上的归档不落地而是报错等下一轮() {
    let f = fixture();
    let a = active();
    let archive = format!("{a}.{STAMP_A}");
    f.node.put(&archive, b"complete\n");
    // 让 read 失败，模拟拉了一半 / 通路出问题。
    f.node.fail_reads.lock().unwrap().push(archive.clone());

    let first = f.sync().await;
    assert_eq!(first.archives_fetched, 0);
    assert_eq!(first.errors.len(), 1, "{first:?}");
    assert!(!f.mirror.dir().join(&archive).exists(), "不许留下一份看起来正常的截断文件");
    assert!(
        !f.mirror.dir().join(format!("{archive}.partial")).exists(),
        "失败时连 .partial 都不该留下"
    );

    // 下一轮恢复，自己补上。
    f.node.fail_reads.lock().unwrap().clear();
    let second = f.sync().await;
    assert_eq!(second.archives_fetched, 1);
    assert_eq!(f.local(&archive), b"complete\n");
}

#[tokio::test]
async fn 一个文件失败不影响别的文件() {
    let f = fixture();
    let a = active();
    let mine = format!("{a}.{STAMP_A}");
    let theirs = format!("{}.{STAMP_A}", audit::active_file_name("u_example_02"));
    f.node.put(&mine, b"MINE\n");
    f.node.put(&theirs, b"THEIRS\n");
    f.node.fail_reads.lock().unwrap().push(mine.clone());

    let report = f.sync().await;
    assert_eq!(report.archives_fetched, 1);
    assert_eq!(report.errors.len(), 1);
    assert_eq!(f.local(&theirs), b"THEIRS\n", "别人的文件照常拉");
}

#[tokio::test]
async fn 节点报的无法归类条目数原样带上来() {
    let f = fixture();
    *f.node.skipped.lock().unwrap() = 3;
    assert_eq!(f.sync().await.skipped_on_node, 3);
}

#[tokio::test]
async fn 同步器从不删除本机文件() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\n");
    f.node.put(&format!("{a}.{STAMP_A}"), b"archived\n");
    f.sync().await;
    let before = f.local_names();

    // 节点侧把归档删了（总量封顶会这么做）。本机**不跟着删**。
    f.node.files.lock().unwrap().remove(&format!("{a}.{STAMP_A}"));
    f.sync().await;
    assert_eq!(f.local_names(), before, "节点删了，本机也不许删");
    assert_eq!(f.local(&format!("{a}.{STAMP_A}")), b"archived\n");
}

#[tokio::test]
async fn 状态在进程之间是持久的() {
    let f = fixture();
    let a = active();
    f.node.put(&a, b"one\n");
    f.sync().await;

    // 换一个 NodeMirror 实例（模拟重启），偏移必须还在——否则重启会把
    // 整个活动文件重收一遍，本机就出现重复行。
    let fresh = NodeMirror::new(f.mirror.dir().parent().unwrap(), "node-example-01").unwrap();
    let report = sync_node("node-example-01", &f.node, &fresh).await.unwrap();
    assert_eq!(report.live_bytes, 0, "重启后不该重收");
    assert_eq!(f.local(&a), b"one\n", "更不该出现重复行");
}

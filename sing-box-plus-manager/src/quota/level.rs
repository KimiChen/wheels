//! 档位的**权限级别**：决定一个人在订阅里看得见哪些节点。
//!
//! # 它只管「看不见」，不管「用不了」
//!
//! 级别不够的节点不进订阅、不在页面上列出。**但这不是拦截。**
//! D11 的账号池让同一个身份名在四台上用的是**同一份凭据**（同一个 uPSK、
//! 同一个 UUID），所以一个曾经拿到过地址的人照样连得上——级别拦不住他。
//!
//! 这是使用者明确选的口径（2026-09-20）：分级是为了引导，不是为了防人。
//! 真要拦住，得在下发时给那台节点上该身份的额度置 0，让节点侧的配额闸拒转
//! （`assemble` 已经会给无主身份 `Limited(0)`，机制是现成的）。那一步没做，
//! 记在这里而不是假装这一版就是访问控制。
//!
//! # 为什么级别不在库里
//!
//! 加列要么重建库、要么走 D32 的四步导入，而这是一个**可见性**功能——
//! 为它把生产账本再重建一次不值。四档是闭集（D23：建库时种下，不存在运行期
//! 新增），所以级别写成与那个闭集并列的常量，加第五档时两处一起改，
//! 而加第五档本来就是一次显式的 schema 变更。
//!
//! 节点那一侧的要求写在 `nodes.toml` 里（`min_level`，缺省 0），
//! 与节点的其余部署事实放在一起。

/// 四档的权限级别。数越大看得越多。
///
/// 留着 10 的间隔是有意的：将来插一档「比 normal 高、比 advanced 低」时
/// 不必重排已有的数，而重排会让所有节点的 `min_level` 一起失去意义。
pub const GROUP_LEVELS: &[(&str, i64)] =
    &[("normal", 0), ("advanced", 10), ("manage", 100), ("admin", 1000)];

/// 某个档位的级别。**认不出的档位按 0 算**。
///
/// 不 panic、也不当成最高级：库里的 `quota_groups` 是带 CHECK 的闭集，
/// 走到这里还认不出，只可能是有人加了档而忘了加级别。那时候把他当成最低级
/// 会让他少看见几个节点（一个看得见的、可报告的故障），
/// 当成最高级则是悄悄把限制解除——两种错里只有前者会被人发现。
pub fn of(group_name: &str) -> i64 {
    GROUP_LEVELS.iter().find(|(name, _)| *name == group_name).map(|(_, level)| *level).unwrap_or(0)
}

/// 看得见这台节点所需的级别。**`nodes.toml` 里没有这台就是 0**。
///
/// 「查不到 = 无要求」而不是「查不到 = 谁都看不见」：往 `server.toml` 加一条
/// 入口却忘了在 `nodes.toml` 里登记，后者会让它从**所有人**的订阅里静默消失，
/// 而前者的后果是「大家都看得见」——一个看得见、说得出口的错。
pub fn required(nodes: &[crate::config::NodeConfig], node_id: &str) -> i64 {
    nodes.iter().find(|node| node.node_id == node_id).map(|node| node.min_level).unwrap_or(0)
}

/// 这个档位看不看得见这台节点。
pub fn can_see(nodes: &[crate::config::NodeConfig], node_id: &str, group_name: &str) -> bool {
    required(nodes, node_id) <= of(group_name)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 四档的级别按顺序递增() {
        let levels: Vec<i64> = GROUP_LEVELS.iter().map(|(_, level)| *level).collect();
        assert_eq!(levels, vec![0, 10, 100, 1000]);
        assert!(levels.windows(2).all(|pair| pair[0] < pair[1]), "级别必须严格递增");
    }

    fn node(node_id: &str, min_level: i64) -> crate::config::NodeConfig {
        let text = format!(
            "node_id = \"{node_id}\"\naddress = \"x:1\"\nfirst_snapshot = \"baseline\"\n\
             snapshot_socket = \"/s\"\nquota_socket = \"/q\"\n\
             agent_spki_sha256 = \"{}\"\nmaterials_dir = \"/m\"\nmin_level = {min_level}\n",
            "0".repeat(64)
        );
        toml::from_str(&text).expect("夹具节点应当解析得动")
    }

    #[test]
    fn 没有登记的节点按无要求处理() {
        // 往 server.toml 加入口却忘了在 nodes.toml 里登记，后果应当是
        // 「大家都看得见」，而不是「从所有人的订阅里静默消失」。
        let nodes = vec![node("known", 100)];
        assert_eq!(required(&nodes, "从没登记过的"), 0);
        assert!(can_see(&nodes, "从没登记过的", "normal"));
    }

    #[test]
    fn 级别不够就看不见() {
        let nodes = vec![node("open", 0), node("premium", 100)];
        assert!(can_see(&nodes, "open", "normal"));
        assert!(!can_see(&nodes, "premium", "normal"));
        assert!(!can_see(&nodes, "premium", "advanced"), "10 < 100");
        assert!(can_see(&nodes, "premium", "manage"), "100 >= 100，等于也算够");
        assert!(can_see(&nodes, "premium", "admin"));
    }

    #[test]
    fn 认不出的档位按最低级算() {
        // 少看见几个节点是可报告的故障；悄悄解除限制不是。
        assert_eq!(of("normal"), 0);
        assert_eq!(of("admin"), 1000);
        assert_eq!(of("将来加的某一档"), 0);
    }
}

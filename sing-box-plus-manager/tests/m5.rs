//! M5 的集成测试入口：泡游 SSO 登录（README §4.9、D27）。
//!
//! | 完成标准 | 模块 |
//! | --- | --- |
//! | state 三段形状、双提交绑定、TTL、前偏、墓碑重放 | [`state`] |
//! | 外呼硬化：只 https、禁重定向、有界、chunked、RFC3986 表单 | [`outbound`] |
//! | check-token 响应形状：code 两形态、data 是数组、逐字段限长 | [`outbound`] |
//! | 名单是授权事实来源：改名单立即生效、`users.role` 不提权 | [`authz`] |
//! | 建号幂等、停用不复活、空 fs_id 不撞唯一索引 | [`authz`] |
//! | 回调顺序：拒未知参数 → 无写入地校验 → 成功后才写墓碑 | [`callback`] |
//! | 一轮对抗式审查逐条落回来的回归 | [`regressions`] |
//! | 订阅 token 生命周期、三个响应细节、凭据缺失 | [`subscription`] |

#[path = "m4/harness.rs"]
mod harness;

#[path = "m5/authz.rs"]
mod authz;
#[path = "m5/callback.rs"]
mod callback;
#[path = "m5/outbound.rs"]
mod outbound;
#[path = "m5/regressions.rs"]
mod regressions;
#[path = "m5/state.rs"]
mod state;
#[path = "m5/subscription.rs"]
mod subscription;

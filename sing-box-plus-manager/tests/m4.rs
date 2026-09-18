//! M4 的集成测试入口。
//!
//! 本轮交付 API 层、会话/CSRF、服务端越权拦截，以及 12 页原型接真实数据。
//! 图表在第三轮。
//!
//! | 完成标准 | 模块 |
//! | --- | --- |
//! | 越权在服务端拦、角色不由客户端声明 | [`api`] |
//! | 会话吊销下一请求生效、成员事实过期拒绝 | [`api`] |
//! | 双提交 CSRF、`If-Match` 乐观并发 | [`api`] |
//! | 字节字段全链路十进制字符串、错误对象形状固定 | [`api`] |
//! | 页面级越权在服务端拦、资源免认证 | [`pages`] |
//! | 前端不出现浮点转换、无内联脚本、kit 与 vendored 库摘要锁定 | [`assets`] |
//! | 趋势桶键与结算侧一致、空桶显式出现、范围上限在 API 层强制 | [`trend`] |

// 趋势用例要让账本行由**真实结算**产生，而不是在测试里直接 INSERT——
// 只有这样才能证明 `hour_bucket` 的写法在结算侧与查询侧逐字一致。
// 键的写法一旦漂移，图会画成一条全零线，看起来像「这段时间没有流量」。
#[path = "m0/fixtures.rs"]
mod fixtures;
#[path = "m1/ledger_harness.rs"]
mod ledger_harness;

#[path = "m4/harness.rs"]
mod harness;

#[path = "m4/api.rs"]
mod api;

#[path = "m4/assets.rs"]
mod assets;

#[path = "m4/pages.rs"]
mod pages;

#[path = "m4/trend.rs"]
mod trend;

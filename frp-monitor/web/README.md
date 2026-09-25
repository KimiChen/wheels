# frp-monitor Web

基于本仓库 `web-standard-kit` 的监控页面，静态资源随 monitor 二进制嵌入发布。
页面与权限设计见根目录 [README.md](../README.md) §7。

实施时复制套件锁定快照到 `assets/`，业务样式与 `src/` 模块（api、stream、store、
format、charts、views）分开维护；不运行时跨子项目引用，不依赖外部 CDN。

页面只能读取服务端已裁剪的视图：公开路由 `/`、`/api/public/v1/*`、`/events/public`
为只读摘要 DTO；管理路由 `/admin/`、`/api/admin/v1/*`、`/events/admin` 需会话认证。
不得包含节点/Dashboard 凭据，移除 `data-demo-submit` 式伪保存。

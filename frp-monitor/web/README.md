# Web

原 `overlay/web/` 已迁到本目录。当前 P0 尚无页面或嵌入入口；构建工具已预留映射到
`extension/frpmonitor/web`，不能把基础二进制描述为已有监控网页。

P1 开始从根 README 锁定的 `web-standard-kit` 快照复制资源到 `assets/`，记录来源。
业务采用原生 ES Modules，样式与套件分离，保留 `wsk-`、`@layer wsk` 和主题/键盘契约。
资源随 monitor 嵌入，不依赖运行时跨子项目文件或外部 CDN。

公开页面只消费服务端裁剪后的 DTO；管理入口及完整详情使用独立会话认证。
秒级 SSE 更新保留筛选、焦点和滚动；大整数由浏览器 DTO 的十进制字符串读取。
当前 Go agent 协议是内部机器协议，禁止将其完整报告直接返回公开网页。

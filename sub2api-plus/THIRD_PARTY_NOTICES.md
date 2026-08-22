# Third-Party Notices

## Sub2API

本项目以 Overlay 方式修改以下上游项目：

- Project: Sub2API
- Source: https://github.com/Wei-Shaw/sub2api
- Ref: `main`
- Source commit: 以 `upstream.lock` 的 `commit` 字段为准
- License: LGPL-3.0

完整上游源码不保存在本仓库中。准备构建时，工具必须取得 `upstream.lock` 固定的
提交并验证身份，再把 `overlay/` 中的完整文件覆盖到临时工作树。修改后的源码、
容器镜像或其他分发物仍须遵守上游许可证及其适用义务。

当前 Overlay 的主要定制范围包括：

- 独立访客门户、公开认证入口和公开路由隔离；
- 网关流量与上游流量统计；
- 多 API 基础地址选择和定制前端入口；
- fork 版本、构建、部署及健康检查定制；
- 与上述能力配套的数据库迁移、配置、测试和文档。

包含真实基础设施信息的 fork 运维文档不属于本公开子项目，也不随 Overlay 分发。

升级上游时，应同步更新本文件、`upstream.lock` 和 Overlay 兼容性结论。

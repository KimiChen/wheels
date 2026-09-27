# Packaging

`python3 scripts/frp.py package` 生成 Linux amd64/arm64 P2 发布包。
参数及前置条件见 [scripts](../scripts/README.md)。每个包包含两个二进制、
固定上游记录、构建信息、校验和及许可证声明，不读取运行时配置。

当前二进制保留原生 FRP，通过配置显式启用监控、探测及历史。网页与纯 Go SQLite
均随二进制提供，不要求运行机安装 Node 或 SQLite。附带 Apache-2.0、采集参考 MIT
及 SQLite 依赖的许可原文。受控本地初始化见 `scripts/local.py`。
systemd、备份、生产安装/认证管理和公开上线验收属于 P3，当前没有生产部署文件。

打包时应只包含自定义 frpc/frps 二进制、必要的静态资源、配置示例、许可证和第三方
声明；不得包含上游临时工作树、Token、真实服务器地址或本地 `.env`。

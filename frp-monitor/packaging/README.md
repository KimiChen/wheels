# Packaging

`python3 scripts/frp.py package` 生成 Linux amd64/arm64 基础发布包。
参数及前置条件见 [scripts](../scripts/README.md)。每个包包含两个二进制、
固定上游记录、构建信息、校验和及许可证声明，不读取运行时配置。

当前 P0 二进制尚无监控功能，包仅用于构建和 FRP 基线验收。
systemd、备份、安装配置生成、认证初始化和公开上线验收属于 P3，当前没有部署文件。

打包时应只包含自定义 frpc/frps 二进制、必要的静态资源、配置示例、许可证和第三方
声明；不得包含上游临时工作树、Token、真实服务器地址或本地 `.env`。

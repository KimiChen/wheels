# Packaging

这里保存 `sub2api-plus` 的公司环境发布模板和部署说明。真实环境配置、域名、IP、
Token、证书、密码、私钥和生成后的部署文件不得提交。

当前生产环境使用 `scripts/build-binary.sh` 生成 Linux 二进制，再通过
`scripts/systemd-release.sh` 发布到已有的 `sub2api.service`。这里不保存真实 unit、
配置文件或服务器连接信息。

Overlay 根 `Dockerfile` 只保留为可选的容器构建入口，不是当前生产部署方式。

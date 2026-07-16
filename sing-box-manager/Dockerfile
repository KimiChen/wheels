# 多阶段构建：Alpine/musl 静态二进制。
# 注意：本镜像只含 sing-box-manager；它通过 SSM API 管理一个独立运行的 sing-box 进程。
FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev gcc
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release

FROM alpine:3
COPY --from=build /src/target/release/sing-box-manager /usr/local/bin/sing-box-manager
# 订阅 API 默认监听端口（按 config.service.listen 调整并 -p 映射）
EXPOSE 9736
ENTRYPOINT ["sing-box-manager"]
CMD ["--help"]

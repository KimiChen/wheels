# 多阶段构建：Alpine/musl 静态二进制。
# 注意：本镜像只含 sing-box-manager，不包含 sing-box。
FROM rust:1.88-alpine AS build
RUN apk add --no-cache musl-dev gcc
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
COPY migrations ./migrations
RUN cargo build --release

FROM alpine:3
COPY --from=build /src/target/release/sing-box-manager /usr/local/bin/sing-box-manager
# Controller 的订阅、健康检查和指标端口。
EXPOSE 9736
ENTRYPOINT ["sing-box-manager"]
CMD ["--help"]

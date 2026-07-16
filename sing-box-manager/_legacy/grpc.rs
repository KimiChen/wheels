//! v2ray_api StatsService gRPC 客户端（reload 模式的每认证身份统计源）。
//! ⚠️ 实测（1.13.14）：虽然 .proto 声明 package `experimental.v2rayapi`，但服务端注册用的
//! ServiceDesc.ServiceName 硬编码成 v2ray-core 的 `v2ray.core.app.stats.command.StatsService`，
//! 所以线路路径是后者——用 experimental.v2rayapi 会 "unknown service"。
//! 手写 prost 消息 + 通用 tonic 客户端，免 .proto/build.rs/protoc。
//! 前提：sing-box 需以 `-tags with_v2ray_api` 构建（官方/homebrew 默认不带）。

use anyhow::Result;
use std::collections::HashMap;

pub mod pb {
    #[derive(Clone, PartialEq, ::prost::Message)]
    pub struct QueryStatsRequest {
        #[prost(string, tag = "1")]
        pub pattern: ::prost::alloc::string::String,
        #[prost(bool, tag = "2")]
        pub reset: bool,
        #[prost(string, repeated, tag = "3")]
        pub patterns: ::prost::alloc::vec::Vec<::prost::alloc::string::String>,
        #[prost(bool, tag = "4")]
        pub regexp: bool,
    }
    #[derive(Clone, PartialEq, ::prost::Message)]
    pub struct Stat {
        #[prost(string, tag = "1")]
        pub name: ::prost::alloc::string::String,
        #[prost(int64, tag = "2")]
        pub value: i64,
    }
    #[derive(Clone, PartialEq, ::prost::Message)]
    pub struct QueryStatsResponse {
        #[prost(message, repeated, tag = "1")]
        pub stat: ::prost::alloc::vec::Vec<Stat>,
    }
}

pub struct V2RayStats {
    grpc: tonic::client::Grpc<tonic::transport::Channel>,
}

impl V2RayStats {
    pub async fn connect(addr: &str) -> Result<Self> {
        let ch = tonic::transport::Endpoint::from_shared(addr.to_string())?
            .connect()
            .await?;
        Ok(Self {
            grpc: tonic::client::Grpc::new(ch),
        })
    }

    /// 拉所有 `user>>>` 计数（reset=false 累计）。返回 (name, up, down)。
    pub async fn user_stats(&mut self) -> Result<Vec<(String, u64, u64)>> {
        self.grpc
            .ready()
            .await
            .map_err(|e| anyhow::anyhow!("gRPC 未就绪：{e}"))?;
        let codec = tonic_prost::ProstCodec::default();
        let path = tonic::codegen::http::uri::PathAndQuery::from_static(
            "/v2ray.core.app.stats.command.StatsService/QueryStats",
        );
        let req = pb::QueryStatsRequest {
            pattern: String::new(),
            reset: false,
            patterns: vec!["user>>>".into()],
            regexp: false,
        };
        let resp: tonic::Response<pb::QueryStatsResponse> = self
            .grpc
            .unary(tonic::Request::new(req), path, codec)
            .await?;

        // user>>>NAME>>>traffic>>>uplink|downlink
        let mut map: HashMap<String, (u64, u64)> = HashMap::new();
        for s in resp.into_inner().stat {
            let parts: Vec<&str> = s.name.split(">>>").collect();
            if parts.len() == 4 && parts[0] == "user" && parts[2] == "traffic" {
                let e = map.entry(parts[1].to_string()).or_default();
                let v = s.value.max(0) as u64;
                match parts[3] {
                    "uplink" => e.0 = v,
                    "downlink" => e.1 = v,
                    _ => {}
                }
            }
        }
        Ok(map.into_iter().map(|(n, (u, d))| (n, u, d)).collect())
    }
}

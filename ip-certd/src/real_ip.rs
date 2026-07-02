use crate::config::ServerConfig;
use axum::http::{header::HeaderName, HeaderMap};
use std::net::IpAddr;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum RealIpError {
    #[error("configured real IP header is invalid")]
    InvalidHeaderName,
    #[error("trusted proxy request is missing real IP header")]
    MissingHeader,
    #[error("real IP header contains invalid IP address")]
    InvalidHeaderValue,
}

pub fn resolve(
    headers: &HeaderMap,
    remote_ip: IpAddr,
    server: &ServerConfig,
) -> Result<IpAddr, RealIpError> {
    if !server.trusted_proxies.contains(&remote_ip) {
        return Ok(remote_ip);
    }

    let header_name = HeaderName::from_bytes(server.real_ip_header.trim().as_bytes())
        .map_err(|_| RealIpError::InvalidHeaderName)?;
    let value = headers
        .get(header_name)
        .ok_or(RealIpError::MissingHeader)?
        .to_str()
        .map_err(|_| RealIpError::InvalidHeaderValue)?;
    parse_real_ip_value(value).ok_or(RealIpError::InvalidHeaderValue)
}

fn parse_real_ip_value(value: &str) -> Option<IpAddr> {
    value
        .split(',')
        .next()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .and_then(|value| value.parse::<IpAddr>().ok())
}

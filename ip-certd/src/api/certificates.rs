use crate::{
    api::errors::ApiError,
    real_ip,
    service::{BundleResponse, IpCertService},
};
use axum::{
    body::Body,
    extract::{connect_info::ConnectInfo, Path, State},
    http::{header, HeaderMap, Response, StatusCode},
};
use std::{net::SocketAddr, sync::Arc};

pub async fn bundle(
    State(service): State<Arc<IpCertService>>,
    Path(ip): Path<String>,
    ConnectInfo(remote_addr): ConnectInfo<SocketAddr>,
    headers: HeaderMap,
) -> Result<Response<Body>, ApiError> {
    let source_ip = real_ip::resolve(&headers, remote_addr.ip(), &service.config().server)
        .map_err(ApiError::from_real_ip)?;
    let bundle = service.certificate_bundle(&ip, source_ip).await?;
    Ok(bundle_response(bundle))
}

fn bundle_response(bundle: BundleResponse) -> Response<Body> {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/gzip")
        .header(
            header::CONTENT_DISPOSITION,
            format!("attachment; filename=\"{}\"", bundle.filename),
        )
        .header("X-Certificate-Hostname", bundle.hostname)
        .header("X-Certificate-IP", bundle.ip.to_string())
        .header("X-Certificate-Not-After", bundle.not_after)
        .body(Body::from(bundle.archive))
        .expect("valid certificate bundle response")
}

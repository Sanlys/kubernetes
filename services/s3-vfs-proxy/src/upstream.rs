use crate::sigv4;
use bytes::Bytes;
use reqwest::{Method, StatusCode};

/// Thin, signed HTTP client for the real Ceph RGW endpoint. Every request
/// this service makes upstream is fully buffered (see main.rs's body-size
/// limit) - simpler and more robust than trying to stream+sign at the
/// same time, at the cost of holding one request/response body in memory
/// at a time. Fine for a personal homelab's file sizes; not meant for
/// multi-GB uploads.
#[derive(Clone)]
pub struct Upstream {
    client: reqwest::Client,
    endpoint: String,
    host_header: String,
    region: String,
    access_key: String,
    secret_key: String,
}

pub struct UpstreamResponse {
    pub status: StatusCode,
    pub headers: reqwest::header::HeaderMap,
    pub body: Bytes,
}

impl Upstream {
    pub fn new(endpoint: &str, region: &str, access_key: &str, secret_key: &str) -> Self {
        let host_header = endpoint
            .trim_start_matches("https://")
            .trim_start_matches("http://")
            .trim_end_matches('/')
            .to_string();
        Self {
            client: reqwest::Client::new(),
            endpoint: endpoint.trim_end_matches('/').to_string(),
            host_header,
            region: region.to_string(),
            access_key: access_key.to_string(),
            secret_key: secret_key.to_string(),
        }
    }

    /// `path` is the raw (unencoded) logical path, e.g. "/media-library/foo bar.txt".
    /// `extra_headers` are attached to the request AND included in the
    /// SigV4 signature (e.g. x-amz-copy-source).
    pub async fn request(
        &self,
        method: Method,
        path: &str,
        query: &[(String, String)],
        extra_headers: &[(String, String)],
        body: Bytes,
    ) -> Result<UpstreamResponse, reqwest::Error> {
        let body_hash = {
            use sha2::{Digest, Sha256};
            let mut hasher = Sha256::new();
            hasher.update(&body);
            hex::encode(hasher.finalize())
        };

        let signed_extra: Vec<(&str, &str)> = extra_headers
            .iter()
            .map(|(k, v)| (k.as_str(), v.as_str()))
            .collect();

        let signed = sigv4::sign(
            method.as_str(),
            &self.host_header,
            path,
            query,
            &signed_extra,
            Some(&body_hash),
            &self.access_key,
            &self.secret_key,
            &self.region,
        );

        // Reuse the exact same encoding used for the SigV4 canonical
        // request - if this URL were encoded differently than what we
        // signed, RGW would recompute a different canonical query string
        // and reject the signature.
        let mut url = format!("{}{}", self.endpoint, sigv4::uri_encode(path, false));
        if !query.is_empty() {
            let mut pairs = query.to_vec();
            pairs.sort();
            let qs: Vec<String> = pairs
                .iter()
                .map(|(k, v)| format!("{}={}", sigv4::uri_encode(k, true), sigv4::uri_encode(v, true)))
                .collect();
            url.push('?');
            url.push_str(&qs.join("&"));
        }

        let mut req = self
            .client
            .request(method, &url)
            .header("host", &self.host_header)
            .header("x-amz-date", &signed.amz_date)
            .header("x-amz-content-sha256", &signed.content_sha256)
            .header("authorization", &signed.authorization);
        for (k, v) in extra_headers {
            req = req.header(k, v);
        }
        if !body.is_empty() {
            req = req.body(body);
        }

        let resp = req.send().await?;
        let status = resp.status();
        let headers = resp.headers().clone();
        let body = resp.bytes().await?;
        Ok(UpstreamResponse {
            status,
            headers,
            body,
        })
    }
}

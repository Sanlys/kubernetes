use crate::sigv4;
use bytes::Bytes;
use futures_util::Stream;
use reqwest::{Method, Response, StatusCode};
use std::pin::Pin;

/// Signed HTTP client for the real Ceph RGW endpoint.
///
/// Small control-plane payloads (list/delete/multipart-control XML) are
/// buffered and hashed for a real SigV4 signature - they're a few KB at
/// most, buffering them is free. Actual file data (PutObject/UploadPart
/// request bodies, GetObject response bodies) is streamed end to end
/// with constant memory regardless of file size, using SigV4's
/// UNSIGNED-PAYLOAD mode so we never need the whole body in hand just to
/// hash it. See `Payload::Streamed`.
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

pub type ByteStream = Pin<Box<dyn Stream<Item = Result<Bytes, axum::Error>> + Send>>;

pub enum Payload {
    Empty,
    Buffered(Bytes),
    /// A request body we forward without ever buffering it whole.
    /// `content_length` is passed straight through from the client's own
    /// Content-Length header (S3 PutObject/UploadPart always send one).
    Streamed { content_length: u64, stream: ByteStream },
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
    /// SigV4 signature (e.g. x-amz-copy-source). Returns the raw,
    /// unconsumed `reqwest::Response` so the caller can choose to stream
    /// it (file downloads) or buffer it (everything else).
    pub async fn request_raw(
        &self,
        method: Method,
        path: &str,
        query: &[(String, String)],
        extra_headers: &[(String, String)],
        payload: Payload,
    ) -> Result<Response, reqwest::Error> {
        let content_length = match &payload {
            Payload::Streamed { content_length, .. } => Some(*content_length),
            _ => None,
        };

        let (body_hash, body): (Option<String>, reqwest::Body) = match payload {
            // Real hash of the empty string, not UNSIGNED-PAYLOAD - these
            // requests (GET/HEAD/DELETE) genuinely have no body, so there's
            // nothing to gain from the streamed variant's shortcut.
            Payload::Empty => {
                use sha2::{Digest, Sha256};
                let hasher = Sha256::new();
                (Some(hex::encode(hasher.finalize())), reqwest::Body::from(Bytes::new()))
            }
            Payload::Buffered(bytes) => {
                use sha2::{Digest, Sha256};
                let mut hasher = Sha256::new();
                hasher.update(&bytes);
                (Some(hex::encode(hasher.finalize())), reqwest::Body::from(bytes))
            }
            // No hash: signed as UNSIGNED-PAYLOAD (see sigv4::sign), so we
            // never have to read the stream twice or buffer it to hash it.
            Payload::Streamed { stream, .. } => (None, reqwest::Body::wrap_stream(stream)),
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
            body_hash.as_deref(),
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
        if let Some(len) = content_length {
            req = req.header(reqwest::header::CONTENT_LENGTH, len);
        }
        req = req.body(body);

        req.send().await
    }

    /// Convenience wrapper over `request_raw` for the small control-plane
    /// calls (list/delete/multipart-control/copy/head) that fully buffer
    /// both the request and response bodies - fine for anything that's
    /// inherently a few KB of XML, never used for file data.
    pub async fn request(
        &self,
        method: Method,
        path: &str,
        query: &[(String, String)],
        extra_headers: &[(String, String)],
        body: Bytes,
    ) -> Result<UpstreamResponse, reqwest::Error> {
        let payload = if body.is_empty() {
            Payload::Empty
        } else {
            Payload::Buffered(body)
        };
        let resp = self.request_raw(method, path, query, extra_headers, payload).await?;
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

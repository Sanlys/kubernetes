mod config;
mod sigv4;
mod upstream;
mod xml;

use axum::body::{Body, Bytes};
use axum::extract::{DefaultBodyLimit, Path, State};
use axum::http::{header, HeaderMap, StatusCode, Uri};
use axum::response::Response;
use axum::routing::get;
use axum::Router;
use reqwest::Method;
use std::collections::HashMap;
use std::sync::Arc;

use config::Config;
use upstream::{ByteStream, Payload, Upstream, UpstreamResponse};

#[derive(Clone)]
struct AppState {
    config: Arc<Config>,
    upstream: Upstream,
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let config_path =
        std::env::var("CONFIG_PATH").unwrap_or_else(|_| "/etc/s3-vfs-proxy/config.json".to_string());
    let config = Config::load(&config_path).expect("failed to load config file");

    let access_key = std::env::var("UPSTREAM_ACCESS_KEY_ID")
        .expect("UPSTREAM_ACCESS_KEY_ID must be set");
    let secret_key = std::env::var("UPSTREAM_SECRET_ACCESS_KEY")
        .expect("UPSTREAM_SECRET_ACCESS_KEY must be set");

    let upstream = Upstream::new(&config.upstream_endpoint, &config.region, &access_key, &secret_key);

    let port: u16 = std::env::var("PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(8080);

    tracing::info!(
        virtual_bucket = %config.virtual_bucket,
        buckets = ?config.buckets,
        "starting s3-vfs-proxy on 0.0.0.0:{port}"
    );

    let state = AppState {
        config: Arc::new(config),
        upstream,
    };

    let app = Router::new()
        .route("/", get(list_buckets))
        .route(
            "/:vbucket",
            get(bucket_get).head(bucket_head).post(bucket_post),
        )
        .route(
            "/:vbucket/*key",
            get(object_get)
                .head(object_head)
                .put(object_put)
                .delete(object_delete)
                .post(object_post),
        )
        // File data (PutObject/UploadPart/GetObject) is streamed with
        // constant memory (see upstream.rs's `Payload::Streamed`), so
        // there's no memory reason to cap request size. Axum's own
        // default (2MB) would otherwise silently reject any upload past
        // it - disable it and let RGW's own bucket/quota limits be the
        // real ceiling.
        .layer(DefaultBodyLimit::disable())
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(("0.0.0.0", port))
        .await
        .expect("failed to bind listener");
    axum::serve(listener, app).await.expect("server error");
}

// ---- query / path helpers ----

fn parse_query(uri: &Uri) -> Vec<(String, String)> {
    uri.query()
        .map(|q| {
            url::form_urlencoded::parse(q.as_bytes())
                .into_owned()
                .collect()
        })
        .unwrap_or_default()
}

fn has_query_flag(query: &[(String, String)], key: &str) -> bool {
    query.iter().any(|(k, _)| k == key)
}

fn replace_query_value(query: &mut Vec<(String, String)>, key: &str, value: &str) {
    if let Some(entry) = query.iter_mut().find(|(k, _)| k == key) {
        entry.1 = value.to_string();
    } else {
        query.push((key.to_string(), value.to_string()));
    }
}

/// Split "folder/rest/of/key" into ("folder", "rest/of/key"). None if
/// there's no '/' at all yet (i.e. still at the virtual bucket's root).
fn split_prefix(s: &str) -> Option<(&str, &str)> {
    s.split_once('/')
}

fn url_decode(s: &str) -> String {
    percent_encoding::percent_decode_str(s)
        .decode_utf8_lossy()
        .to_string()
}

/// Resolve a (virtual bucket, virtual key) pair down to the real
/// (bucket, key) it maps to, or the error response to send back.
fn resolve(state: &AppState, vbucket: &str, key: &str) -> Result<(String, String), Response> {
    if vbucket != state.config.virtual_bucket {
        return Err(not_found_bucket(vbucket));
    }
    let Some((folder, rest)) = split_prefix(key) else {
        return Err(no_such_key(key));
    };
    let Some(real_bucket) = state.config.resolve_bucket(folder) else {
        return Err(no_such_key(key));
    };
    Ok((real_bucket.to_string(), rest.to_string()))
}

// ---- response helpers ----

fn xml_response(status: StatusCode, body: impl Into<Vec<u8>>) -> Response {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/xml")
        .body(Body::from(body.into()))
        .unwrap()
}

fn s3_error(status: StatusCode, code: &str, message: &str) -> Response {
    let body = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?><Error><Code>{code}</Code><Message>{message}</Message></Error>"#,
    );
    xml_response(status, body.into_bytes())
}

fn not_found_bucket(name: &str) -> Response {
    s3_error(
        StatusCode::NOT_FOUND,
        "NoSuchBucket",
        &format!("no such bucket: {name}"),
    )
}

fn no_such_key(key: &str) -> Response {
    s3_error(StatusCode::NOT_FOUND, "NoSuchKey", &format!("no such key: {key}"))
}

fn bad_request(msg: &str) -> Response {
    s3_error(StatusCode::BAD_REQUEST, "InvalidRequest", msg)
}

fn upstream_error(e: reqwest::Error) -> Response {
    tracing::error!("upstream request failed: {e}");
    s3_error(StatusCode::BAD_GATEWAY, "InternalError", "upstream request failed")
}

/// Pass a real object response (GET/PUT/DELETE/CopyObject/UploadPart)
/// straight through, keeping the handful of headers clients actually
/// care about and the body unchanged.
fn passthrough(r: UpstreamResponse) -> Response {
    let mut builder = Response::builder().status(r.status);
    for name in [
        header::CONTENT_TYPE,
        header::CONTENT_LENGTH,
        header::ETAG,
        header::LAST_MODIFIED,
        header::ACCEPT_RANGES,
        header::CONTENT_RANGE,
    ] {
        if let Some(v) = r.headers.get(&name) {
            builder = builder.header(name, v.clone());
        }
    }
    builder.body(Body::from(r.body)).unwrap()
}

/// Same as `passthrough`, but for a `reqwest::Response` we haven't (and
/// won't) buffer - the body streams straight through to the client with
/// constant memory, regardless of how large the object is. Used for
/// GetObject and for the streamed PutObject/UploadPart responses (which
/// happen to be tiny, but there's no need for a second code path).
fn passthrough_streamed(resp: reqwest::Response) -> Response {
    let status = resp.status();
    let mut builder = Response::builder().status(status);
    for name in [
        header::CONTENT_TYPE,
        header::CONTENT_LENGTH,
        header::ETAG,
        header::LAST_MODIFIED,
        header::ACCEPT_RANGES,
        header::CONTENT_RANGE,
    ] {
        if let Some(v) = resp.headers().get(&name) {
            builder = builder.header(name, v.clone());
        }
    }
    builder.body(Body::from_stream(resp.bytes_stream())).unwrap()
}

/// Same as `passthrough` but for HEAD responses: keep the metadata
/// headers (including the real Content-Length a GET would return) while
/// sending no body, per HEAD semantics.
fn passthrough_head(r: UpstreamResponse) -> Response {
    let mut builder = Response::builder().status(r.status);
    for name in [
        header::CONTENT_TYPE,
        header::CONTENT_LENGTH,
        header::ETAG,
        header::LAST_MODIFIED,
        header::ACCEPT_RANGES,
    ] {
        if let Some(v) = r.headers.get(&name) {
            builder = builder.header(name, v.clone());
        }
    }
    builder.body(Body::empty()).unwrap()
}

// ---- ListBuckets ----

async fn list_buckets(State(state): State<AppState>) -> Response {
    xml_response(
        StatusCode::OK,
        xml::list_buckets(&state.config.virtual_bucket).into_bytes(),
    )
}

// ---- bucket-level ----

async fn bucket_get(State(state): State<AppState>, Path(vbucket): Path<String>, uri: Uri) -> Response {
    if vbucket != state.config.virtual_bucket {
        return not_found_bucket(&vbucket);
    }
    let query = parse_query(&uri);
    let qmap: HashMap<&str, &str> = query.iter().map(|(k, v)| (k.as_str(), v.as_str())).collect();
    let prefix = qmap.get("prefix").copied().unwrap_or("");

    match split_prefix(prefix) {
        None => {
            let folders: Vec<&str> = state.config.buckets.iter().map(|s| s.as_str()).collect();
            xml_response(
                StatusCode::OK,
                xml::top_level_listing(&state.config.virtual_bucket, prefix, &folders).into_bytes(),
            )
        }
        Some((folder, rest)) => {
            let Some(real_bucket) = state.config.resolve_bucket(folder) else {
                return xml_response(
                    StatusCode::OK,
                    xml::top_level_listing(&state.config.virtual_bucket, prefix, &[]).into_bytes(),
                );
            };
            let mut upstream_query = query.clone();
            replace_query_value(&mut upstream_query, "prefix", rest);
            let path = format!("/{real_bucket}");
            match state
                .upstream
                .request(Method::GET, &path, &upstream_query, &[], Bytes::new())
                .await
            {
                Ok(r) => {
                    let folder_prefix = format!("{folder}/");
                    let rewritten =
                        xml::rewrite_list_objects(&r.body, &state.config.virtual_bucket, &folder_prefix);
                    xml_response(r.status, rewritten)
                }
                Err(e) => upstream_error(e),
            }
        }
    }
}

async fn bucket_head(State(state): State<AppState>, Path(vbucket): Path<String>) -> Response {
    if vbucket != state.config.virtual_bucket {
        return not_found_bucket(&vbucket);
    }
    Response::builder()
        .status(StatusCode::OK)
        .body(Body::empty())
        .unwrap()
}

async fn bucket_post(
    State(state): State<AppState>,
    Path(vbucket): Path<String>,
    uri: Uri,
    body: Bytes,
) -> Response {
    if vbucket != state.config.virtual_bucket {
        return not_found_bucket(&vbucket);
    }
    let query = parse_query(&uri);
    if !has_query_flag(&query, "delete") {
        return bad_request("unsupported bucket-level POST");
    }

    let keys = xml::parse_delete_keys(&body);
    let mut groups: HashMap<String, Vec<String>> = HashMap::new();
    let mut fragments: Vec<String> = Vec::new();

    for key in &keys {
        match split_prefix(key) {
            Some((folder, rest)) if state.config.resolve_bucket(folder).is_some() => {
                groups.entry(folder.to_string()).or_default().push(rest.to_string());
            }
            _ => {
                fragments.push(xml::error_fragment(key, "NoSuchKey", "unknown bucket"));
            }
        }
    }

    for (folder, rel_keys) in groups {
        let real_bucket = state.config.resolve_bucket(&folder).unwrap().to_string();
        let rel_key_refs: Vec<&str> = rel_keys.iter().map(|s| s.as_str()).collect();
        let req_body = xml::build_delete_request(&rel_key_refs);
        let path = format!("/{real_bucket}");
        let del_query = vec![("delete".to_string(), String::new())];
        match state
            .upstream
            .request(Method::POST, &path, &del_query, &[], Bytes::from(req_body))
            .await
        {
            Ok(r) => {
                let folder_prefix = format!("{folder}/");
                let rewritten = xml::rewrite_delete_response(&r.body, &folder_prefix);
                fragments.push(xml::inner_of_delete_result(&rewritten));
            }
            Err(e) => {
                tracing::error!("upstream delete failed for bucket {folder}: {e}");
                for k in rel_keys {
                    fragments.push(xml::error_fragment(
                        &format!("{folder}/{k}"),
                        "InternalError",
                        "upstream request failed",
                    ));
                }
            }
        }
    }

    xml_response(StatusCode::OK, xml::wrap_delete_result(&fragments).into_bytes())
}

// ---- object-level ----

async fn object_get(
    State(state): State<AppState>,
    Path((vbucket, key)): Path<(String, String)>,
    headers: HeaderMap,
) -> Response {
    let (real_bucket, real_key) = match resolve(&state, &vbucket, &key) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let mut extra = Vec::new();
    if let Some(range) = headers.get(header::RANGE) {
        if let Ok(v) = range.to_str() {
            extra.push(("Range".to_string(), v.to_string()));
        }
    }
    let path = format!("/{real_bucket}/{real_key}");
    // Streamed both ways: this is the actual file data, so it's forwarded
    // with constant memory rather than buffered - see upstream.rs.
    match state.upstream.request_raw(Method::GET, &path, &[], &extra, Payload::Empty).await {
        Ok(resp) => passthrough_streamed(resp),
        Err(e) => upstream_error(e),
    }
}

async fn object_head(
    State(state): State<AppState>,
    Path((vbucket, key)): Path<(String, String)>,
) -> Response {
    let (real_bucket, real_key) = match resolve(&state, &vbucket, &key) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let path = format!("/{real_bucket}/{real_key}");
    match state.upstream.request(Method::HEAD, &path, &[], &[], Bytes::new()).await {
        Ok(r) => passthrough_head(r),
        Err(e) => upstream_error(e),
    }
}

async fn object_put(
    State(state): State<AppState>,
    Path((vbucket, key)): Path<(String, String)>,
    uri: Uri,
    headers: HeaderMap,
    // Must be the last extractor: it takes the raw, unbuffered request
    // body so PutObject/UploadPart can stream straight through to RGW
    // (see upstream.rs) instead of holding the whole file in memory.
    body: Body,
) -> Response {
    let (real_bucket, real_key) = match resolve(&state, &vbucket, &key) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let query = parse_query(&uri);
    let path = format!("/{real_bucket}/{real_key}");

    // CopyObject: server-side, no request body involved at all.
    if let Some(copy_source) = headers.get("x-amz-copy-source") {
        let cs = url_decode(copy_source.to_str().unwrap_or_default());
        let trimmed = cs.trim_start_matches('/');
        let Some((src_vbucket, src_rest)) = trimmed.split_once('/') else {
            return bad_request("invalid x-amz-copy-source");
        };
        if src_vbucket != state.config.virtual_bucket {
            return bad_request("copy source must be within this virtual bucket");
        }
        let Some((src_folder, src_key)) = split_prefix(src_rest) else {
            return bad_request("invalid x-amz-copy-source");
        };
        let Some(src_real_bucket) = state.config.resolve_bucket(src_folder) else {
            return bad_request("unknown source bucket in x-amz-copy-source");
        };
        let new_source = format!("/{src_real_bucket}/{src_key}");
        let extra = vec![("x-amz-copy-source".to_string(), new_source)];
        return match state.upstream.request(Method::PUT, &path, &[], &extra, Bytes::new()).await {
            Ok(r) => passthrough(r),
            Err(e) => upstream_error(e),
        };
    }

    // Plain PutObject or UploadPart: stream the body through. S3 clients
    // always send a Content-Length for these (no chunked-encoding
    // uploads), so there's a real length to hand upstream even though we
    // never buffer the bytes ourselves.
    let content_length = headers
        .get(header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<u64>().ok())
        .unwrap_or(0);
    let stream: ByteStream = Box::pin(body.into_data_stream());
    let payload = Payload::Streamed { content_length, stream };
    let upload_query = if has_query_flag(&query, "uploadId") {
        query
    } else {
        Vec::new()
    };
    match state.upstream.request_raw(Method::PUT, &path, &upload_query, &[], payload).await {
        Ok(resp) => passthrough_streamed(resp),
        Err(e) => upstream_error(e),
    }
}

async fn object_delete(
    State(state): State<AppState>,
    Path((vbucket, key)): Path<(String, String)>,
    uri: Uri,
) -> Response {
    let (real_bucket, real_key) = match resolve(&state, &vbucket, &key) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let query = parse_query(&uri);
    let path = format!("/{real_bucket}/{real_key}");
    match state.upstream.request(Method::DELETE, &path, &query, &[], Bytes::new()).await {
        Ok(r) => passthrough(r),
        Err(e) => upstream_error(e),
    }
}

async fn object_post(
    State(state): State<AppState>,
    Path((vbucket, key)): Path<(String, String)>,
    uri: Uri,
    body: Bytes,
) -> Response {
    let (real_bucket, real_key) = match resolve(&state, &vbucket, &key) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let query = parse_query(&uri);
    let path = format!("/{real_bucket}/{real_key}");
    let folder = key.split('/').next().unwrap_or("");
    let folder_prefix = format!("{folder}/");

    if has_query_flag(&query, "uploads") {
        return match state.upstream.request(Method::POST, &path, &query, &[], Bytes::new()).await {
            Ok(r) => {
                let rewritten =
                    xml::rewrite_multipart_response(&r.body, &state.config.virtual_bucket, &folder_prefix);
                xml_response(r.status, rewritten)
            }
            Err(e) => upstream_error(e),
        };
    }

    if has_query_flag(&query, "uploadId") {
        return match state.upstream.request(Method::POST, &path, &query, &[], body).await {
            Ok(r) => {
                let rewritten =
                    xml::rewrite_multipart_response(&r.body, &state.config.virtual_bucket, &folder_prefix);
                xml_response(r.status, rewritten)
            }
            Err(e) => upstream_error(e),
        };
    }

    bad_request("unsupported object POST")
}

FROM rust:1-slim AS builder
WORKDIR /build
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release

FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/target/release/s3-vfs-proxy /usr/local/bin/s3-vfs-proxy
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/s3-vfs-proxy"]

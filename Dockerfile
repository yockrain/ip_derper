FROM golang:latest AS builder

WORKDIR /app

# ========= CONFIG =========
# - download links
ENV MODIFIED_DERPER_GIT=https://github.com/yockrain/ip_derper.git
ENV BRANCH=ip_derper
# ==========================

# build modified derper
RUN git clone -b $BRANCH $MODIFIED_DERPER_GIT tailscale --depth 1 && \
    cd /app/tailscale/cmd/derper && \
    /usr/local/go/bin/go build -ldflags "-s -w" -o /app/derper && \
    cd /app && \
    rm -rf /app/tailscale

FROM ubuntu:20.04
WORKDIR /app

# ========= CONFIG =========
# - derper args
ENV DERP_HOST=127.0.0.1
ENV DERP_CERTS=/app/certs/
ENV DERP_ADDR :30070
ENV DERP_STUN true
ENV DERP_HTTP_PORT 30080
ENV DERP_VERIFY_CLIENTS false
# ==========================

# apt
RUN apt-get update && \
    apt-get install -y openssl curl

COPY build_cert.sh /app/
COPY --from=builder /app/derper /app/derper

# build self-signed certs && start derper
CMD bash /app/build_cert.sh $DERP_HOST $DERP_CERTS /app/san.conf && \
    /app/derper --hostname=$DERP_HOST \
    --certmode=manual \
    --certdir=$DERP_CERTS \
    --stun=$DERP_STUN  \
    --verify-clients=$DERP_VERIFY_CLIENTS

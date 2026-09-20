FROM golang:1.24-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o SAgent .

# Vector 下载（静态二进制）
FROM alpine:3.19 AS vector-downloader
RUN wget -qO /tmp/vector.tar.gz \
    https://github.com/vectordotdev/vector/releases/download/v0.34.2/vector-0.34.2-x86_64-unknown-linux-musl.tar.gz && \
    tar -xzf /tmp/vector.tar.gz -C /tmp && \
    mv /tmp/vector-x86_64-unknown-linux-musl/bin/vector /tmp/vector

# mysqld_exporter 下载
FROM alpine:3.19 AS exporter-downloader
RUN wget -qO /tmp/mysqld_exporter.tar.gz \
    https://github.com/prometheus/mysqld_exporter/releases/download/v0.15.1/mysqld_exporter-0.15.1.linux-amd64.tar.gz && \
    tar -xzf /tmp/mysqld_exporter.tar.gz -C /tmp && \
    mv /tmp/mysqld_exporter-0.15.1.linux-amd64/mysqld_exporter /tmp/mysqld_exporter

# 最终镜像
FROM alpine:3.19
RUN adduser -D -h /home/deploy deploy && \
    mkdir -p /home/deploy/SAgent && \
    chown -R deploy:deploy /home/deploy
USER deploy
WORKDIR /home/deploy/SAgent
RUN mkdir -p bin plugins/vector/bin plugins/mysql_exporter plugins/custom_scripts conf/plugins/vector data/wal data/checkpoints logs run
COPY --from=builder --chown=deploy:deploy /build/SAgent bin/SAgent
COPY --from=vector-downloader --chown=deploy:deploy /tmp/vector plugins/vector/bin/vector
COPY --from=exporter-downloader --chown=deploy:deploy /tmp/mysqld_exporter plugins/mysql_exporter/mysqld_exporter
COPY --chown=deploy:deploy conf/plugins/vector/ conf/plugins/vector/
COPY --chown=deploy:deploy plugins/custom_scripts/ plugins/custom_scripts/
COPY --chown=deploy:deploy run.sh .
USER root
RUN chmod +x /home/deploy/SAgent/run.sh
USER deploy
EXPOSE 19090
ENTRYPOINT ["./run.sh"]
STOPSIGNAL SIGTERM
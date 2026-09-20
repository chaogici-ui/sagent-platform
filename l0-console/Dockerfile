# l0-console — SAgent 中心控制台
FROM golang:1.25-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o l0-console .

FROM alpine:3.19
# docker-cli：容器内通过挂载的 docker.sock 探测本机 l1-* 演示 Agent 状态
RUN apk add --no-cache docker-cli && \
    adduser -D -h /home/deploy deploy && \
    mkdir -p /home/deploy/l0-console && \
    chown -R deploy:deploy /home/deploy
USER deploy
WORKDIR /home/deploy/l0-console
RUN mkdir -p static data
COPY --from=builder --chown=deploy:deploy /build/l0-console ./l0-console
COPY --chown=deploy:deploy static/ static/
COPY --chown=deploy:deploy data/integrations/ data/integrations/
# 指标种子（系统内置插件 host_metrics/docker_metrics/custom_scripts 的完整治理字段定义）
COPY --chown=deploy:deploy data/metrics.json data/metrics.json
EXPOSE 8080
ENV VM_URL=http://victoriametrics:8428
ENTRYPOINT ["./l0-console"]

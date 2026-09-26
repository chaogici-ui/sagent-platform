# vm-host —— 模拟真实业务主机的靶机（SAgent 接入目标）
# 独立干净主机：只有 sshd + python3（ansible 执行器依赖），预装任何 SAgent 产物
FROM alpine:3.19
RUN apk add --no-cache openssh python3 && \
    adduser -D -s /bin/sh deploy && \
    mkdir -p /var/run/sshd
COPY start.sh /start.sh
RUN chmod +x /start.sh
EXPOSE 22
ENTRYPOINT ["/start.sh"]

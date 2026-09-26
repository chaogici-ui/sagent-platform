# l0-console — SAgent 中心控制台
FROM alpine:3.19
# docker-cli：容器内通过挂载的 docker.sock 探测本机 l1-* 演示 Agent 状态
# ansible-core + sshpass + openssh-client：平台执行器——主机探路（ping/setup）与
# SAgent 安装（playbook）。openssh-client 必须显式装（alpine 不随 ansible-core 带入，
# 缺它时 sshpass 报 "Failed to run command"，远端全部 unreachable）
# 依赖预编译二进制（宿主机交叉编译产物 COPY 进来），不做容器内 go build（VM 内存红线）
RUN apk add --no-cache docker-cli ansible-core sshpass openssh-client && \
    adduser -D -h /home/deploy deploy && \
    mkdir -p /home/deploy/l0-console && \
    chown -R deploy:deploy /home/deploy
USER deploy
WORKDIR /home/deploy/l0-console
RUN mkdir -p static data
COPY --chown=deploy:deploy bin/l0-console-linux-arm64 ./l0-console
COPY --chown=deploy:deploy static/ static/
COPY --chown=deploy:deploy data/integrations/ data/integrations/
# 指标种子（系统内置插件 host_metrics/docker_metrics/custom_scripts 的完整治理字段定义）
COPY --chown=deploy:deploy data/metrics.json data/metrics.json
COPY --chown=deploy:deploy data/onboard_config.json data/onboard_config.json
# 流程模板（L2 层）：模式编排数据文件，改流程不改代码
COPY --chown=deploy:deploy data/flow_templates/ data/flow_templates/
# SAgent 版本清单 + 平台本地版本库（安装作业的分发源，sha256 与清单互相校验）
COPY --chown=deploy:deploy data/versions.yaml data/versions.yaml
COPY --chown=deploy:deploy data/binaries/ data/binaries/
# 安装 playbook（L2 层）：改安装动作不改代码
COPY --chown=deploy:deploy data/playbooks/ data/playbooks/
# pristine 种子副本：data 卷遮蔽镜像文件，entrypoint 每次启动用种子同步到卷（镜像赢/卷赢分离）
COPY --chown=deploy:deploy data/metrics.json data/onboard_config.json data/versions.yaml /opt/l0-seed/
COPY --chown=deploy:deploy data/integrations/ /opt/l0-seed/integrations/
COPY --chown=deploy:deploy data/flow_templates/ /opt/l0-seed/flow_templates/
COPY --chown=deploy:deploy data/binaries/ /opt/l0-seed/binaries/
COPY --chown=deploy:deploy data/playbooks/ /opt/l0-seed/playbooks/
COPY --chown=deploy:deploy docker-entrypoint.sh ./docker-entrypoint.sh
RUN chmod +x ./docker-entrypoint.sh
EXPOSE 8080
ENV VM_URL=http://victoriametrics:8428
ENTRYPOINT ["./docker-entrypoint.sh"]

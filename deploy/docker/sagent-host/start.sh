#!/bin/sh
# 模拟真实业务主机的启动引导：配置 sshd（账号密码认证）后前台常驻
# 凭据由环境变量注入（SSH_DEPLOY_PASSWORD），容器内不落任何我方产物——
# SAgent 由平台 ansible 执行器安装（接入流水线 install_agent 环节）
set -e

ssh-keygen -A >/dev/null 2>&1
echo "deploy:${SSH_DEPLOY_PASSWORD:-sagent123}" | chpasswd
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config

# 前台运行 sshd（PID 1，同时充当容器保活进程）
exec /usr/sbin/sshd -D -e

#!/usr/bin/env bash
# ============================================================================
#  L1 采集机「四进程」一键打包脚本（架构 D5 IN4）
#
#  产出：<ARCHIVE_DIR>/l1-edge-4proc-<VERSION>-<GOOS>-<GOARCH>.tar.gz
#  内容：l0-console 单一二进制（4 进程_{PackageCache/Gateway/Controller/AnsibleRunner}
#        经不同 -flag 起不同进程）+ 4 个 systemd/前台启动脚本 + 依赖数据种子 +
#        部署说明 README。
#
#  用法：
#    ./build-l1-bundle.sh                 # 用当前工作区已编译的二进制打包
#    ./build-l1-bundle.sh --rebuild       # 先交叉编译本机可达平台的二进制再打包
#    ./build-l1-bundle.sh --version 1.2.3 # 覆盖产物版本号（缺省取 SA_VERSION env 或 0.4.0）
#  依赖目录约定：从 l0-console/ 取 data 种子（versions.yaml/playbooks/binaries/integrations）。
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."          # 回到仓库根

L1_SRC="l0-console"
ARCHIVE_DIR="${L1_ARCHIVE_DIR:-dist/l1-bundle}"
VERSION="${SA_VERSION:-0.4.0}"
GOOS="${GOOS:-linux}"

# 平台/架构：--rebuild 时自动探测本机，或由环境覆盖
GOARCH="${GOARCH:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"

# ---- 入口参数 ----
REBUILD=0
for arg in "$@"; do
  case "$arg" in
    --rebuild) REBUILD=1 ;;
    --version=*) VERSION="${arg#--version=}" ;;
  esac
done

# ---- 1) 二进制：校验或交叉编译 ----
if [[ $REBUILD -eq 1 ]]; then
  echo "[pack] 交叉编译 l0-console (GOOS=$GOOS GOARCH=$GOARCH, version=$VERSION)..."
  (cd "$L1_SRC" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
     go build -o "bin/l0-console-$GOOS-$GOARCH" .)
  BIN="$L1_SRC/bin/l0-console-$GOOS-$GOARCH"
else
  BIN="$L1_SRC/bin/l0-console-$GOOS-$GOARCH"
  if [[ ! -x "$BIN" ]]; then
    echo "!! 未找到 $BIN；请先 --rebuild 交叉编译，或将已编译二进制放到该路径。"
    exit 1
  fi
fi

# ---- 2) 组装目录 ----
STAGE=$(mktemp -d)/l1-edge
mkdir -p "$STAGE/bin" "$STAGE/etc/systemd" "$STAGE/seed"

echo "[pack] 组装四进程 bundle (version=$VERSION, $GOOS/$GOARCH)..."
cp "$BIN" "$STAGE/bin/l0-console"
chmod 0755 "$STAGE/bin/l0-console"

# 四个角色启动脚本：同一二进制的四套前方参数（systemd 用 ExecStart，前台脚本演示用）
write_role() { # $1=role $2=flag $3=port
  cat > "$STAGE/bin/start-$1.sh" <<EOF
#!/usr/bin/env bash
# L1 进程 $1 (port $3)：$2
exec "\$(dirname "\$0")/l0-console" $2
EOF
  chmod 0755 "$STAGE/bin/start-$1.sh"

  cat > "$STAGE/etc/systemd/l1-$1.service" <<EOF
[Unit]
Description=L1 采集机进程 $1 (架构 D5)
After=network.target

[Service]
Type=simple
ExecStart=/opt/l1-edge/bin/l0-console $2
Restart=always
Environment=L1_PACKAGE_CACHE=/var/lib/l1-edge/packages
Environment=L0_CONSOLE_URL=http://127.0.0.1:8460

[Install]
WantedBy=multi-user.target
EOF
}

write_role package-cache  "-package-cache" 8450
write_role gateway        "-gateway"       8460
write_role controller     "-controller"    8461
write_role ansible-runner "-ansible-runner" 8462

# 数据种子：版本清单/playbook/二进制库/integrations（Runner 就近取包 + 安装脚本所需）
cp "$L1_SRC/data/versions.yaml"      "$STAGE/seed/versions.yaml"
cp -r "$L1_SRC/data/playbooks"       "$STAGE/seed/playbooks"
cp -r "$L1_SRC/data/binaries"        "$STAGE/seed/binaries"
cp -r "$L1_SRC/data/integrations"    "$STAGE/seed/integrations"
cp "$L1_SRC/data/onboard_config.json" "$STAGE/seed/onboard_config.json" 2>/dev/null || true

# 安装脚本：把 seed 落到 /opt/l1-edge/data 并起 4 个 systemd 服务
cat > "$STAGE/install.sh" <<'EOF'
#!/usr/bin/env bash
# L1 采集机部署：安装四进程到 /opt/l1-edge + 4 个 systemd 单元
set -euo pipefail
PREFIX="${L1_PREFIX:-/opt/l1-edge}"
echo "[install] 安装 L1 四进程到 $PREFIX ..."
install -d "$PREFIX/bin" "$PREFIX/etc/systemd" "$PREFIX/data"
cp -r bin/l0-console bin/start-*.sh "$PREFIX/bin/"
cp -r seed/versions.yaml seed/playbooks seed/binaries seed/integrations seed/onboard_config.json "$PREFIX/data/"
cp etc/systemd/l1-*.service /etc/systemd/system/
systemctl daemon-reload
for svc in package-cache gateway controller ansible-runner; do
  systemctl enable --now "l1-$svc.service" || echo "  !! l1-$svc 启动失败（请 journalctl -u l1-$svc 排查）"
done
echo "[install] 完成：4 进程已由 systemd 托管。状态：systemctl status l1-{package-cache,gateway,controller,ansible-runner}"
EOF
chmod 0755 "$STAGE/install.sh"

cat > "$STAGE/README.md" <<'READMEEOF'
# L1 采集机「四进程」部署包（架构 D5 IN4）

版本：__VERSION__（__GOOS__/__GOARCH__）

## 包内 4 个进程（共用单一二进制，不同 flag）
| 进程 | 启动 flag | 默认端口 | 职责 |
|------|-----------|---------|------|
| Package Cache | `-package-cache` | 8450 | 版本包只读镜像 + 签名校验 + 就近分发 |
| Gateway       | `-gateway`       | 8460 | 控制通道汇聚中继 |
| Controller    | `-controller`    | 8461 | 任务拉取 / 分流 / 回执 |
| Ansible Runner| `-ansible-runner`| 8462 | 安装 playbook 执行端 |

## 部署
```bash
sudo ./install.sh
```
默认安装到 /opt/l1-edge，用 systemd 托管 4 进程（另可手动：
`bin/start-package-cache.sh` 等前台启动）。

## 关键环境变量
- L0_CONSOLE_URL：上游 L0 基址（默认 http://127.0.0.1:8460 即走本机 Gateway）
- L1_PACKAGE_CACHE：打包缓存根（默认 /var/lib/l1-edge/packages）
- L1_CONTROLLER_ID / L1_ANSIBLE_RUNNER_ID / L1_CONTROLLER_LOOP_MS 等进程身份/节奏可配
READMEEOF
sed -i '' -e "s/__VERSION__/$VERSION/g; s/__GOOS__/$GOOS/g; s/__GOARCH__/$GOARCH/g" "$STAGE/README.md"

# ---- 3) 打 tar.gz ----
ARCHIVE_NAME="l1-edge-4proc-${VERSION}-${GOOS}-${GOARCH}.tar.gz"
ROOT="$PWD"  # 仓库根（脚本开头已 cd），归档输出到绝对路径，避免 tar 子 shell 相对路径错位
mkdir -p "$ROOT/$ARCHIVE_DIR"
( cd "$STAGE/.." && tar -czf "$ROOT/$ARCHIVE_DIR/$ARCHIVE_NAME" l1-edge )
echo "[pack] 完成：$ROOT/$ARCHIVE_DIR/$ARCHIVE_NAME"
echo "[pack] 内容："
( cd "$STAGE" && find . -type f | sort )
rm -rf "$(dirname "$STAGE")"
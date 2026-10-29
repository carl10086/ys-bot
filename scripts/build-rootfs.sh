#!/bin/bash
# build-rootfs.sh — 把 agentd 烤进 rootfs 模板（在 VM 宿主上以 root 跑一次）。
# 借鉴 e2b"envd 烤进模板镜像"；流水线不抄，脚本够了。
# 用法: bash build-rootfs.sh <base-rootfs> <agentd-binary> <out-template>
set -euo pipefail

BASE=${1:?base rootfs}
AGENTD=${2:?agentd binary (static, linux/amd64)}
OUT=${3:?output template}

cp --reflink=auto "$BASE" "$OUT"
MNT=$(mktemp -d)
trap 'umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true' EXIT
mount -o loop "$OUT" "$MNT"

install -m 0755 "$AGENTD" "$MNT/usr/local/bin/agentd"

# minimized rootfs 缺 CA 证书（curl https 报 error 77）：从宿主补一份
mkdir -p "$MNT/etc/ssl/certs"
cp -L /etc/ssl/certs/ca-certificates.crt "$MNT/etc/ssl/certs/" 2>/dev/null || \
  cp -L /etc/pki/tls/certs/ca-bundle.crt "$MNT/etc/ssl/certs/ca-certificates.crt"

# 网络配置脚本：从内核 cmdline 读 ys_ip/ys_gw（vmhostd 起 VM 时按 slot 写入 boot_args）。
# 不用 netplan/MMDS：cmdline 是 firecracker 原生通道，零依赖。
cat > "$MNT/usr/local/bin/ys-net-setup.sh" <<'NET'
#!/bin/bash
set -e
for arg in $(cat /proc/cmdline); do
  case "$arg" in
    ys_ip=*) IP="${arg#ys_ip=}" ;;
    ys_gw=*) GW="${arg#ys_gw=}" ;;
  esac
done
[ -n "${IP:-}" ] || exit 0   # 没传 ys_ip = 不配网（兼容冒烟 rootfs）
ip addr replace "$IP" dev eth0
ip link set eth0 up
ip route replace default via "$GW"
printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' > /etc/resolv.conf
NET
chmod 0755 "$MNT/usr/local/bin/ys-net-setup.sh"

cat > "$MNT/etc/systemd/system/agentd.service" <<'UNIT'
[Unit]
Description=ys agentd (tool execution daemon)
After=local-fs.target

[Service]
ExecStartPre=/usr/local/bin/ys-net-setup.sh
ExecStart=/usr/local/bin/agentd
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT
mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
ln -sf /etc/systemd/system/agentd.service "$MNT/etc/systemd/system/multi-user.target.wants/agentd.service"

umount "$MNT"
trap - EXIT
rmdir "$MNT"
echo "OK: $OUT"

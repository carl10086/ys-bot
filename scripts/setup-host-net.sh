#!/bin/bash
# setup-host-net.sh — VM 宿主的一次性网络初始化（root 跑）。
# VM 网段 10.200.0.0/16（每 slot 一个 /30）；NAT 出公网。
set -euo pipefail

sysctl -w net.ipv4.ip_forward=1
grep -q '^net.ipv4.ip_forward=1' /etc/sysctl.conf || echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf

# NAT：VM 出公网（幂等：-C 检查后再 -A）
iptables -t nat -C POSTROUTING -s 10.200.0.0/16 -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -s 10.200.0.0/16 -j MASQUERADE
iptables -C FORWARD -s 10.200.0.0/16 -j ACCEPT 2>/dev/null || \
  iptables -A FORWARD -s 10.200.0.0/16 -j ACCEPT
iptables -C FORWARD -d 10.200.0.0/16 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
  iptables -A FORWARD -d 10.200.0.0/16 -m state --state RELATED,ESTABLISHED -j ACCEPT

echo "OK: ip_forward=1, NAT for 10.200.0.0/16"

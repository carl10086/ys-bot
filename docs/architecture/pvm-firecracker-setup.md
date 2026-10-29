# 沙箱主机环境：腾讯云 CVM + PVM + Firecracker（已验证）

> 状态：**2026-10-29 全部实测通过**。结论先行：普通腾讯云 CVM（无嵌套虚拟化）可以跑真 Firecracker，
> 路线 = PVM 宿主内核 + PVM guest 内核；快照/恢复也验证通过，M4 无技术障碍。

## 机器

- 新开的腾讯云 CVM（北京六区）：4 核 8G / 50G，OpenCloudOS 9.6，按量计费
- `ssh root@152.136.212.59`（root 直连）
- ⚠️ 不要用 `lingo-ansible` 做实验：它是生产堡垒机（JumpServer/Jenkins/禅道），重启不可接受

## 为什么是 PVM

腾讯云普通 CVM 屏蔽嵌套虚拟化（`/proc/cpuinfo` 无 vmx/svm 标志，`/dev/kvm` 不存在）。
裸金属（黑石 BMS）¥1 万+/月，排除。PVM（Pagetable-based VM，SOSP'23 论文出身，腾讯生产在用）
在 guest 内核层用影子页表实现虚拟化，**不需要宿主 hypervisor 配合**，装上内核即得 `/dev/kvm`。

## 三个已验证的关键事实（别的地方没写清的）

1. **宿主**：OpenCloudOS 9 的 yum 仓库直接有 PVM 内核（extras 源），无需手工下 rpm
2. **guest 内核必须是 PVM 版**（`vmlinux-pvm`）：普通 guest 内核在 PVM 下 KVM_RUN 立即
   triple fault（Firecracker 报 `Unexpected exit reason on vcpu run: Shutdown`，无任何串口输出）。
   官方下载页 "自建部署" 小节有 vmlinux-pvm-amd64 的直链（cnb.cool，国内直连快）
3. **原版 Firecracker 可以在 PVM 上跑**：v1.17.0 实测，不需要 CubeSandbox 全栈

## 复现步骤（在新 OpenCloudOS 9 机器上从零到 microVM）

```bash
# 1. PVM 宿主内核（extras 源）
dnf install -y kernel-6.6.117-45.16.cubesandbox.oc9
grubby --info=ALL | grep -E "^kernel=|^index="   # 找到 cubesandbox 内核的 index
grubby --set-default-index=<index>
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/pvm/grub/host_grub_config.sh | bash
reboot

# 2. 验证 + 开机自动加载
ls -l /dev/kvm || modprobe kvm_pvm
echo kvm_pvm > /etc/modules-load.d/kvm-pvm.conf

# 3. Firecracker binary（GitHub 直连慢，本地下载后 scp）
mkdir -p /data/firecracker && cd /data/firecracker
# firecracker v1.17.0: github releases → firecracker-v1.17.0-x86_64.tgz → 解压取 firecracker-*-x86_64

# 4. guest 内核 + rootfs
curl -fsSL -o vmlinux-pvm "https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/kernel-release-260812-1/vmlinux-pvm-amd64"
# rootfs: firecracker quickstart 的 bionic.rootfs.ext4（S3，国内慢，本地中转 scp）

# 5. 起 VM / 快照 / 恢复
bash /data/firecracker/boot-vm.sh      # API 起 VM，console 输出到 console.log
bash /data/firecracker/restore-vm.sh   # 从快照恢复
```

## 冒烟实测结果（2026-10-29）

| 验证项 | 结果 |
|---|---|
| PVM 内核启动、`modprobe kvm_pvm` 后 `/dev/kvm` | ✓ |
| Firecracker v1.17.0 + vmlinux-pvm 起 microVM（1 vCPU / 512MB） | ✓ boot 到 root shell |
| Pause → Full snapshot（snap.mem 512MB + snap.state 14KB） | ✓ |
| 新进程 snapshot/load → Resume | ✓ resume 耗时 51µs |

## 文件位置（机器上）

`/data/firecracker/`：`firecracker`（binary）、`vmlinux-pvm`、`rootfs.ext4`、
`boot-vm.sh` / `restore-vm.sh`、`fc.sock`、`console*.log`、`snap.*`

## 参考

- CubeSandbox 文档（PVM 部署 / 下载页）：`https://github.com/TencentCloud/CubeSandbox`（docs/zh/guide/pvm-deploy.md, downloads.md）
- Firecracker quickstart：`https://github.com/firecracker-microvm/firecracker`
- 注意：CubeSandbox 自家 VMM 是 Cloud Hypervisor 系（不是 Firecracker），我们只用它的 PVM 内核 + guest 内核

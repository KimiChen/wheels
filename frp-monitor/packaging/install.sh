#!/usr/bin/env bash
set -euo pipefail

# frp-monitor 安装工具：安装二进制、示例配置与 systemd 单元。
# 只安装骨架，绝不覆盖已有配置；秘密（token/密码摘要）由管理员按示例自行填写。

usage() {
  printf '用法：sudo %s <server|agent> <发布包解压目录>\n' "$(basename "$0")" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
role="$1"; dist="$2"
[[ "$role" == "server" || "$role" == "agent" ]] || usage
[[ "$(id -u)" == "0" ]] || { echo "需要 root（sudo）" >&2; exit 1; }

bin="frp-monitor-$role"
[[ -x "$dist/$bin" ]] || { echo "发布包中缺少 $bin" >&2; exit 1; }

install -m 0755 "$dist/$bin" "/usr/local/bin/$bin"
install -d -m 0755 /etc/frp-monitor

cfg="frps.toml"; [[ "$role" == "agent" ]] && cfg="frpc.toml"
if [[ ! -e "/etc/frp-monitor/$cfg" ]]; then
  install -m 0600 "$dist/packaging/$cfg.example" "/etc/frp-monitor/$cfg"
  echo "已安装示例配置 /etc/frp-monitor/$cfg，请先编辑再启动服务。"
else
  echo "保留已有配置 /etc/frp-monitor/$cfg（示例见 $dist/packaging/$cfg.example）。"
fi

if [[ "$role" == "server" ]]; then
  install -d -m 0700 /var/lib/frp-monitor
  if [[ ! -e /var/lib/frp-monitor/credentials.json ]]; then
    install -m 0600 "$dist/packaging/credentials.json.example" /var/lib/frp-monitor/credentials.json
    echo "已安装示例凭据文件 /var/lib/frp-monitor/credentials.json，请用管理端或自行替换。"
  fi
fi

install -m 0644 "$dist/packaging/$bin.service" "/etc/systemd/system/$bin.service"
systemctl daemon-reload

cat <<EOF
完成。后续：
  1. 编辑 /etc/frp-monitor/$cfg（FRP 认证、monitor 段；agent 的 token 建议环境变量）
  2. systemctl enable --now $bin
  3. 检查：journalctl -u $bin -f
EOF

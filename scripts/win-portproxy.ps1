# win-portproxy.ps1 — WSL 端口转发（替代不稳定的 WSL localhostForwarding）
# 需管理员 PowerShell 运行。获取当前 WSL IP 并重建 portproxy（仅绑 127.0.0.1，
# 不对外网暴露）。WSL 重启后 IP 变化时重跑本脚本即可。
#
# 用法：powershell -ExecutionPolicy Bypass -File scripts\win-portproxy.ps1
# 可选注册开机自动刷新（管理员）：
#   schtasks /Create /TN DocFlow-WSL-Portproxy /SC ONSTART /RU SYSTEM /
#     /TR "powershell -NoProfile -ExecutionPolicy Bypass -File <仓库绝对路径>\scripts\win-portproxy.ps1"

$ErrorActionPreference = 'Stop'

# WSL VM IP（NAT 模式网卡首个地址；docker 网桥 172.17/18.x 已排除——取最大非 172.17/18 段地址不可靠，
# WSL 主网卡地址是 hostname -I 输出的第一个）
$wslIp = (wsl -e hostname -I).Trim().Split(' ')[0]
if (-not ($wslIp -match '^\d+\.\d+\.\d+\.\d+$')) {
    throw "无法获取 WSL IP：'$wslIp'（确认 WSL 已启动）"
}

# 转发端口表：caddy(80/443)、backend 直连(18081/18082)、mailpit UI(18025)
$ports = 80, 443, 18081, 18082, 18025

foreach ($p in $ports) {
    netsh interface portproxy delete v4tov4 listenport=$p listenaddress=127.0.0.1 | Out-Null
    netsh interface portproxy add v4tov4 listenport=$p listenaddress=127.0.0.1 connectport=$p connectaddress=$wslIp | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "portproxy $p 添加失败（需管理员权限，且 IP Helper 服务运行中）" }
}

Write-Output "WSL IP: $wslIp"
netsh interface portproxy show all

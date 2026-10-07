# 服务管理模式示例

演示如何将 VRNDIS-Go 配置为操作系统服务，实现开机自启动和后台运行。

## 服务安装

### Linux (systemd)

```bash
# 安装服务
sudo ./build/bin/vrndisd service install \
    --mode server \
    --network 10.77.0.0/24 \
    --gateway 10.77.0.1 \
    --listen 0.0.0.0:7777

# 启动服务
sudo systemctl enable vrndis-server
sudo systemctl start vrndis-server

# 查看状态
sudo systemctl status vrndis-server

# 停止服务
sudo systemctl stop vrndis-server

# 卸载服务
sudo ./build/bin/vrndisd service uninstall
```

### Windows

```cmd
# 安装服务
.\build\bin\vrndisd.exe service install ^
    --mode server ^
    --network 10.77.0.0/24 ^
    --gateway 10.77.0.1 ^
    --listen 0.0.0.0:7777

# 启动服务
sc start vrndisd

# 查看状态
sc query vrndisd

# 停止服务
sc stop vrndisd

# 卸载服务
.\build\bin\vrndisd.exe service uninstall
```

## 服务配置

### 配置文件位于

**Linux**: `/etc/vrndis/config.json`  
**Windows**: `%APPDATA%\vrndis\config.json`

### 配置示例

```json
{
  "server": {
    "listen": "0.0.0.0:7777",
    "network": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "tapName": "vrndis-srv",
    "enableNAT": true
  },
  "service": {
    "restartOnCrash": true,
    "logLevel": "info",
    "logFile": "/var/log/vrndis/vrndis.log"
  }
}
```

## 日志文件位置

- **Linux**: `/var/log/vrndis/vrndis.log`
- **Windows**: `%APPDATA%\vrndis\vrndis.log`

## 常用服务操作

```bash
# 服务是否运行
sudo systemctl is-active vrndis-server

# 查看服务日志
sudo journalctl -u vrndis-server -f

# 重启服务
sudo systemctl restart vrndis-server

# 服务配置加载
sudo service vrndis-server reload

# 服务详情
service vrndis-server --version
```

## 生产环境部署

### 环境变量
```
VrndisServerListen=0.0.0.0:7777
VrndisNetwork=10.77.0.0/24
VrndisGateway=10.77.0.1
VrndisTapName=vrndis-srv
```

### Docker 部署
```bash
docker run -d --name vrndis-server \
  --net host \
  -v /dev:/dev \
  -v /var/log/vrndis:/var/log/vrndis \
  vrndis/vrndis-server:1.0.0
```

## 监控指标

- 服务运行状态
- 在线客户端数量
- 网络流量统计
- CPU/内存使用率

## 故障排查

1. 服务无法启动
   ```bash
   # 检查日志
   sudo journalctl -u vrndis-server -n 100
   ```

2. 端口冲突
   ```bash
   # 检查端口占用
   ss -tlnp | grep 7777
   ```

3. TAP 设备权限
   ```bash
   # 添加用户组
   sudo usermod -aG netdev $USER
   ```

## 升级和维护

```bash
# 备份当前配置
cp /etc/vrndis/config.json /etc/vrndis/config.json.bak

# 重新安装新版本服务
./build/bin/vrndisd service uninstall
./build/bin/vrndisd service install --version 1.0.0+newhash
```

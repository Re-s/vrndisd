# VRNDIS-Go 使用示例

这组示例展示了 VRNDIS-Go 的各种使用场景和配置方式。

## 示例目录

1. [基本连接示例](./basic-connection/) - 最简单的服务器-客户端连接
2. [STUN P2P 示例](./stun-p2p/) - 使用 STUN 服务端NAT打洞的P2P连接
3. [多客户端网络](./multi-client/) - 多个客户端组成的虚拟网络
4. [服务管理模式](./service-mode/) - 作为系统服务运行
5. [管理命令使用](./management-commands/) - 使用 status/clients 管理命令
6. [故障排查](./troubleshooting/) - 常见问题的调试方法

## 快速开始

```bash
# 克隆项目
git clone https://github.com/yourname/vrndis-go.git
cd vrndis-go

# 查看示例
ls examples/

# 进入示例
cd examples/basic-connection
./setup.sh
```

## 依赖要求

- Go 1.21+
- Linux/Unix 系统（部分示例支持 Windows）
- 需要 root 权限（用于 TAP 设备配置）

## 每个示例包含

- 说明文档（README.md）
- 自动化脚本（setup.sh, cleanup.sh）
- 配置模板
- 测试步骤

## 许可证

MIT License

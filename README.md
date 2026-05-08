# deployctl

一个轻量 Go 批量 SSH/SFTP 运维工具，可替代简单的 `sshpass + rsync + ssh` 批量脚本。

## 功能

- 支持 SSH 密码认证，类似 sshpass。
- 支持 SSH 私钥认证。
- 支持 YAML 配置，不同机器可以有不同 user、password、password_env、port、key。
- 支持批量配置免密，不依赖 `ssh-copy-id`。
- 支持批量取消本工具配置的免密。
- 支持批量上传目录，不依赖 `rsync` / `scp`，使用 Go SFTP 实现。
- 支持批量执行远程命令。
- 支持并发控制。
- 支持生成默认配置。

## 编译

```bash
go mod tidy
go build -o deployctl .
```

## 快速使用

```bash
./deployctl init -o config.yaml
export SSHPASS='你的root密码'
./deployctl trust-add -c config.yaml
./deployctl deploy -c config.yaml
./deployctl exec -c config.yaml --cmd "hostname && uptime"
./deployctl copy -c config.yaml --src AnyBackupClient --remote-dir /opt
./deployctl trust-remove -c config.yaml
```

## 替代原脚本

原逻辑：

```bash
rsync -avz AnyBackupClient root@10.71.43.x:/opt/
ssh root@10.71.43.x "cd /opt/AnyBackupClient && chmod +x install-silent.sh && ./install-silent.sh"
```

替代命令：

```bash
./deployctl deploy -c config.yaml
```

区别：本工具用 SFTP 递归上传，不是 rsync 差异同步协议。

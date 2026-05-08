# deployctl

`deployctl` 是一个轻量的 Go 批量 SSH/SFTP 运维工具，适合在多台 Linux 主机上批量配置 SSH 免密、复制文件或目录、执行远程命令，以及执行“复制后运行命令”的部署流程。

它不依赖 `ssh-copy-id`、`rsync`、`scp` 等外部工具，主要通过 Go SSH/SFTP 实现。

## 功能特性

- 支持 SSH 密码认证，可用于类似 `sshpass` 的场景。
- 支持 SSH 私钥认证。
- 支持 YAML 配置多台主机。
- 支持每台主机单独配置 `user`、`port`、`password`、`password_env`、`key`。
- 支持批量配置 SSH 免密，不依赖 `ssh-copy-id`。
- 支持批量取消由本工具配置的 SSH 免密。
- 支持批量复制本地文件或目录到远端主机。
- 支持批量执行远程命令。
- 支持复制文件或目录后再执行远程命令。
- 支持隐藏执行模式：并发执行，最后汇总每台主机结果。
- 支持可见执行模式：单进程逐台执行，实时显示远程命令输出，便于观察脚本过程，也便于随时中断。
- 支持生成默认配置文件。

## 构建

```bash
go mod tidy
go build -o deployctl .
```

交叉编译 Linux amd64：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o deployctl-linux-amd64 .
```

也可以使用 GitHub Actions 中的手动构建工作流：

```text
Actions -> Build deployctl -> Run workflow
```

## 生成默认配置

```bash
./deployctl init -o config.yaml
```

如果配置文件已经存在，需要覆盖：

```bash
./deployctl init -o config.yaml -force
```

## 配置文件示例

```yaml
concurrency: 5
timeout: 30s

defaults:
  user: root
  port: 22
  password_env: "SSHPASS"
  key: "~/.ssh/deployctl_id_rsa"

trust:
  managed_key: "~/.ssh/deployctl_id_rsa"

deploy:
  src_dir: "local-package"
  remote_dir: "/opt"
  command: "cd /opt/local-package && chmod +x install.sh && ./install.sh"
  mode: hidden

hosts:
  - host: 192.168.1.10
  - host: 192.168.1.11
  - host: 192.168.1.12
```

不同主机可以覆盖默认认证信息：

```yaml
hosts:
  - host: 192.168.1.10

  - host: 192.168.1.11
    port: 2222
    password: "host-specific-password"

  - host: 192.168.1.12
    user: admin
    password_env: "HOST_12_PASS"

  - host: 192.168.1.13
    key: "~/.ssh/custom_id_rsa"
```

## 密码认证

统一密码可以使用环境变量，避免把密码明文写入配置：

```bash
export SSHPASS='your-password'
./deployctl exec -c config.yaml --cmd "hostname && uptime"
```

也可以在 YAML 中给单台主机配置 `password` 或 `password_env`。

## 批量配置 SSH 免密

```bash
export SSHPASS='your-password'
./deployctl trust-add -c config.yaml
```

默认会在本机生成或复用：

```text
~/.ssh/deployctl_id_rsa
~/.ssh/deployctl_id_rsa.pub
```

然后把公钥追加到远端用户的：

```text
~/.ssh/authorized_keys
```

本工具写入的 key 会带有 `deployctl-managed` 标记，便于后续安全删除。

## 批量取消 SSH 免密

```bash
./deployctl trust-remove -c config.yaml
```

它只会删除由 `deployctl` 管理的 key，不会清空远端 `authorized_keys`。

如果当前私钥已经不可用，也可以使用密码认证删除：

```bash
export SSHPASS='your-password'
./deployctl trust-remove -c config.yaml
```

## 批量执行命令

隐藏模式，适合多台主机并发执行：

```bash
./deployctl exec -c config.yaml --cmd "hostname && uptime" --mode hidden
```

可见模式，适合需要观察执行过程的命令或脚本：

```bash
./deployctl exec -c config.yaml --cmd "bash /tmp/task.sh" --mode visible
```

两种模式都会在最后输出汇总信息，包括每台主机的执行状态和退出码。

## 批量复制文件或目录

复制目录：

```bash
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt
```

复制单个文件：

```bash
./deployctl copy -c config.yaml --src ./tool.sh --remote-dir /tmp
```

远端路径会自动使用本地文件或目录的 basename。例如 `./local-package` 会复制到 `/opt/local-package`。

## 复制后执行命令

使用配置文件里的 `deploy` 段：

```bash
./deployctl deploy -c config.yaml
```

临时覆盖源路径、远端目录、命令和执行模式：

```bash
./deployctl deploy \
  -c config.yaml \
  --src ./local-package \
  --remote-dir /opt \
  --cmd "cd /opt/local-package && chmod +x install.sh && ./install.sh" \
  --mode visible
```

## 执行模式说明

### hidden

```bash
./deployctl deploy -c config.yaml --mode hidden
```

特点：

- 按 `concurrency` 并发执行。
- 远程输出会在命令结束后显示。
- 最后汇总每台主机的结果和退出码。
- 适合稳定、无需人工观察的批量任务。

### visible

```bash
./deployctl deploy -c config.yaml --mode visible
```

特点：

- 单进程逐台执行。
- 远程 stdout/stderr 实时显示在当前终端。
- 适合安装脚本、初始化脚本、长时间任务。
- 运行过程中可以用 `Ctrl+C` 中断。

## 常见流程

```bash
# 1. 生成配置
./deployctl init -o config.yaml

# 2. 编辑 config.yaml 中的 hosts、认证信息、部署参数
vim config.yaml

# 3. 使用密码批量配置免密
export SSHPASS='your-password'
./deployctl trust-add -c config.yaml

# 4. 使用免密批量复制并执行部署命令
./deployctl deploy -c config.yaml --mode visible

# 5. 如需取消由本工具配置的免密
./deployctl trust-remove -c config.yaml
```

## 注意事项

- 文件复制使用 SFTP 递归上传，不是 rsync 差异同步协议。
- 默认跳过严格的 known_hosts 校验，适合内网初始化和批量运维场景；生产环境如需更高安全性，建议后续增加 known_hosts 校验。
- `visible` 模式适合观察过程；`hidden` 模式适合批量并发。
- 不建议把真实密码直接写进配置文件，优先使用 `password_env`。

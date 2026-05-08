# deployctl

`deployctl` 是一个用 Go 编写的轻量批量 SSH/SFTP/rsync 运维工具。它面向“我有一批 Linux 主机，需要批量复制文件、执行命令、安装脚本、配置 SSH 免密”的场景。

它的目标不是替代 Ansible 这类完整配置管理系统，而是提供一个简单、单文件可执行、配置清晰、适合内网批量初始化和批量部署的小工具。


## 适合哪些场景

常见适用场景：

- 一批新机器需要批量配置 SSH 免密。
- 一批机器需要批量上传安装包、脚本或配置目录。
- 一批机器需要批量执行初始化命令。
- 希望不用 `ssh-copy-id`、`scp`、`sshpass`，只用一个 Go 可执行文件完成批量操作。
- 机器支持 rsync 时，希望复制目录可以走 rsync；不支持时仍可回退到 SFTP。
- 希望用 YAML 管理主机清单和认证信息。
- 希望安装脚本执行时可以实时看到输出，也能在普通批量任务中并发执行。

## 不适合哪些场景

`deployctl` 不是完整的配置管理系统，不适合复杂的状态编排、幂等资源管理、角色系统、模板渲染、任务依赖编排等复杂场景。需要这些能力时，Ansible、SaltStack、Puppet、Chef 等工具会更合适。

## 核心命令

```bash
# 生成默认配置
./deployctl init -o config.yaml

# 批量配置 SSH 免密
./deployctl trust-add -c config.yaml

# 批量取消由 deployctl 配置的 SSH 免密
./deployctl trust-remove -c config.yaml

# 批量复制文件或目录，默认 SFTP
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt

# 批量复制文件或目录，指定 rsync
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt --copy-method rsync

# 自动选择复制方式：rsync 可用则用 rsync，否则回退 SFTP
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt --copy-method auto

# 批量执行远程命令
./deployctl exec -c config.yaml --cmd "hostname && uptime"

# 按 deploy 配置执行：可复制、可执行、也可复制后执行
./deployctl deploy -c config.yaml
```

## deploy 的行为规则

`deploy` 是一个组合命令，行为由 `deploy` 配置决定。

```yaml
deploy:
  copy_method: sftp
  src_dir: ""
  remote_dir: ""
  command: ""
  mode: hidden
```

规则如下：

| 配置 | 行为 |
|---|---|
| `src_dir` + `remote_dir` + `command` | 先复制，再执行命令 |
| `src_dir` + `remote_dir` | 只复制文件或目录，不执行命令 |
| `command` | 只执行命令，不复制文件 |
| 三者都为空 | 报错提示 |

也就是说，如果你只想复制文件，不要配置 `command`。如果你只想执行命令，不要配置 `src_dir` 和 `remote_dir`。

## 复制方式

`deployctl` 的复制模块支持三种方式：

| copy_method | 说明 |
|---|---|
| `sftp` | 默认方式，纯 Go SFTP 实现，不依赖 rsync，支持密码和私钥认证 |
| `rsync` | 使用系统外部 `rsync` 命令复制，需要本机和远端都安装 rsync，并且 SSH 私钥可用 |
| `auto` | 自动检测 rsync 是否可用；可用则走 rsync，不可用则回退 SFTP |

配置示例：

```yaml
deploy:
  copy_method: auto
  src_dir: "./local-package"
  remote_dir: "/opt"
  command: ""
  mode: hidden
```

命令行临时覆盖：

```bash
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt --copy-method auto
```

注意：`rsync` 模式调用的是本机系统上的 `rsync` 命令，并通过系统 `ssh` 连接远端。因此它需要可用的 SSH 私钥或 SSH agent；密码认证场景建议使用 `sftp` 或 `auto`。`auto` 会在 rsync 条件不满足时回退到 SFTP。

## 执行模式

`deployctl` 支持两种执行模式。

### hidden

```bash
./deployctl deploy -c config.yaml --mode hidden
```

特点：

- 按 `concurrency` 并发执行。
- 远程输出在命令结束后显示。
- 最后汇总每台机器的状态和退出码。
- 适合稳定、不需要实时观察过程的批量任务。

### visible

```bash
./deployctl deploy -c config.yaml --mode visible
```

特点：

- 单进程逐台执行。
- 远程 stdout/stderr 实时显示在当前终端。
- 远程输出也会写入日志文件。
- 适合安装脚本、初始化脚本、长时间任务。
- 执行过程中可以用 `Ctrl+C` 中断。

## 安装和构建

### 从源码构建

```bash
git clone https://github.com/fanhuadesenlinnn/deployctl.git
cd deployctl
go mod tidy
go build -o deployctl ./cmd/deployctl-rsync
```

交叉编译 Linux amd64：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o deployctl-linux-amd64 ./cmd/deployctl-rsync
```

### 使用 GitHub Actions 构建

仓库中提供手动触发的构建工作流：

```text
Actions -> Build deployctl -> Run workflow
```

可以填写版本号，例如：

```text
v0.1.0
```

如果 `publish_release` 为 `true`，构建产物会发布到 GitHub Releases。

## 生成配置文件

```bash
./deployctl init -o config.yaml
```

如果文件已经存在，需要覆盖：

```bash
./deployctl init -o config.yaml -force
```

生成的配置类似：

```yaml
concurrency: 5
timeout: 30s

logging:
  file: "deployctl.log"
  level: "info"

defaults:
  user: root
  port: 22
  password_env: "SSHPASS"
  key: "~/.ssh/deployctl_id_rsa"

trust:
  managed_key: "~/.ssh/deployctl_id_rsa"

deploy:
  copy_method: sftp
  src_dir: ""
  remote_dir: ""
  command: ""
  mode: hidden

hosts:
  - host: 192.168.1.10
  - host: 192.168.1.11
  - host: 192.168.1.12
```

## 配置说明

### 并发和超时

```yaml
concurrency: 5
timeout: 30s
```

`concurrency` 控制 hidden 模式下的并发数量。`timeout` 控制 SSH 连接超时时间。

### 日志

```yaml
logging:
  file: "deployctl.log"
  level: "info"
```

也可以在命令行临时指定日志文件：

```bash
./deployctl deploy -c config.yaml --log-file ./logs/deploy.log
```

详细日志：

```bash
./deployctl deploy -c config.yaml -v
./deployctl deploy -c config.yaml -vv
./deployctl deploy -c config.yaml -vvv
```

说明：

- 默认显示普通信息、警告和错误。
- `-v` 显示认证来源、配置文件、日志文件等调试信息。
- `-vvv` 显示更细的上传过程日志。
- 日志文件会记录屏幕日志、远程输出和执行汇总。

### 默认认证信息

```yaml
defaults:
  user: root
  port: 22
  password_env: "SSHPASS"
  key: "~/.ssh/deployctl_id_rsa"
```

统一密码建议用环境变量：

```bash
export SSHPASS='your-password'
```

也可以直接配置默认密码：

```yaml
defaults:
  user: root
  port: 22
  password: "your-password"
```

不建议把真实密码写进配置文件，除非是在受控环境中临时使用。

### 单台机器覆盖配置

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

密码优先级：

```text
host.password
-> defaults.password
-> env(host.password_env)
-> env(defaults.password_env)
```

如果单台机器没有配置密码，会自动尝试默认密码或默认环境变量。

## 批量配置 SSH 免密

先设置密码环境变量：

```bash
export SSHPASS='your-password'
```

执行：

```bash
./deployctl trust-add -c config.yaml -v
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

## 只复制文件或目录

使用 `copy` 命令：

```bash
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt
```

使用 rsync：

```bash
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt --copy-method rsync
```

自动选择 rsync 或 SFTP：

```bash
./deployctl copy -c config.yaml --src ./local-package --remote-dir /opt --copy-method auto
```

复制单个文件：

```bash
./deployctl copy -c config.yaml --src ./tool.sh --remote-dir /tmp
```

远端路径会自动使用本地文件或目录的 basename。例如 `./local-package` 会复制到 `/opt/local-package`。

也可以使用 `deploy` 做只复制：

```yaml
deploy:
  copy_method: auto
  src_dir: "./local-package"
  remote_dir: "/opt"
  command: ""
  mode: hidden
```

然后执行：

```bash
./deployctl deploy -c config.yaml
```

## 只执行命令

使用 `exec` 命令：

```bash
./deployctl exec -c config.yaml --cmd "hostname && uptime" --mode hidden
```

可见执行：

```bash
./deployctl exec -c config.yaml --cmd "bash /tmp/task.sh" --mode visible
```

也可以使用 `deploy` 做只执行：

```yaml
deploy:
  src_dir: ""
  remote_dir: ""
  command: "hostname && uptime"
  mode: hidden
```

然后执行：

```bash
./deployctl deploy -c config.yaml
```

## 复制后执行命令

配置：

```yaml
deploy:
  copy_method: auto
  src_dir: "./local-package"
  remote_dir: "/opt"
  command: "cd /opt/local-package && chmod +x install.sh && ./install.sh"
  mode: visible
```

执行：

```bash
./deployctl deploy -c config.yaml
```

也可以临时用命令行覆盖：

```bash
./deployctl deploy \
  -c config.yaml \
  --src ./local-package \
  --remote-dir /opt \
  --copy-method auto \
  --cmd "cd /opt/local-package && chmod +x install.sh && ./install.sh" \
  --mode visible
```

## 推荐使用流程

```bash
# 1. 生成配置
./deployctl init -o config.yaml

# 2. 编辑主机清单、认证方式、日志和部署配置
vim config.yaml

# 3. 用密码批量配置免密
export SSHPASS='your-password'
./deployctl trust-add -c config.yaml -v

# 4. 批量复制或部署，优先尝试 rsync，不满足条件自动回退 SFTP
./deployctl deploy -c config.yaml --copy-method auto --mode visible --log-file ./logs/deploy.log

# 5. 需要时取消由 deployctl 配置的免密
./deployctl trust-remove -c config.yaml
```

## 注意事项

- `sftp` 是默认复制方式，不依赖 rsync。
- `rsync` 模式依赖本机和远端都安装 rsync，并且需要可用的 SSH 私钥或 SSH agent。
- 密码认证场景建议使用 `sftp` 或 `auto`；`auto` 会在 rsync 不可用时回退 SFTP。
- 默认跳过严格的 known_hosts 校验，适合内网初始化和批量运维场景。
- 生产环境如需更高安全性，建议后续增加 known_hosts 校验。
- `visible` 模式适合观察过程；`hidden` 模式适合批量并发。
- 不建议把真实密码直接写进配置文件，优先使用 `password_env`。

## License

This project is licensed under the GNU General Public License v3.0. See [LICENSE](LICENSE) for details.

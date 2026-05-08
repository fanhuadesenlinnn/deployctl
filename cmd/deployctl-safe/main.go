package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

const markerPrefix = "deployctl-managed"

type Config struct {
	Concurrency int           `yaml:"concurrency"`
	Timeout     time.Duration `yaml:"timeout"`
	Logging     Logging       `yaml:"logging"`
	Defaults    Auth          `yaml:"defaults"`
	Trust       Trust         `yaml:"trust"`
	Deploy      Deploy        `yaml:"deploy"`
	Hosts       []Node        `yaml:"hosts"`
}

type Logging struct {
	File  string `yaml:"file"`
	Level string `yaml:"level"`
}

type Auth struct {
	User        string `yaml:"user"`
	Port        int    `yaml:"port"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	Key         string `yaml:"key"`
}

type Node struct {
	Host        string `yaml:"host"`
	User        string `yaml:"user"`
	Port        int    `yaml:"port"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	Key         string `yaml:"key"`
}

type Trust struct {
	ManagedKey string `yaml:"managed_key"`
}

type Deploy struct {
	SrcDir     string `yaml:"src_dir"`
	RemoteDir  string `yaml:"remote_dir"`
	Command    string `yaml:"command"`
	Mode       string `yaml:"mode"`
	CopyMethod string `yaml:"copy_method"`
}

type Host struct {
	Name           string
	User           string
	Port           int
	Password       string
	PasswordSource string
	Key            string
	KeySource      string
}

type Result struct {
	Host     string
	ExitCode int
	Err      error
}

type Options struct {
	ConfigPath string
	LogFile    string
	Verbosity  int
}

type Logger struct {
	mu        sync.Mutex
	file      *os.File
	verbosity int
}

var logger = &Logger{}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "错误：程序遇到未预期的问题，已安全退出。\n提示：请使用 -v 或 --log-file 收集日志后反馈。\n")
			os.Exit(2)
		}
	}()

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = runInit(os.Args[2:])
	case "trust-add":
		err = runTrustAdd(os.Args[2:])
	case "trust-remove":
		err = runTrustRemove(os.Args[2:])
	case "copy":
		err = runCopy(os.Args[2:])
	case "exec":
		err = runExec(os.Args[2:])
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("未知命令：%s", os.Args[1])
	}

	logger.close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", friendlyError(err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`deployctl - batch SSH/SFTP/rsync operations for Linux hosts

Usage:
  deployctl init -o config.yaml [-force]
  deployctl trust-add -c config.yaml [-v|-vv|-vvv] [--log-file deployctl.log]
  deployctl trust-remove -c config.yaml [-v|-vv|-vvv] [--log-file deployctl.log]
  deployctl copy -c config.yaml --src ./local-package --remote-dir /opt [--copy-method sftp|rsync|auto]
  deployctl exec -c config.yaml --cmd "hostname && uptime" --mode hidden|visible
  deployctl deploy -c config.yaml [--src ./local-package] [--remote-dir /opt] [--cmd "..."] [--copy-method sftp|rsync|auto]

Copy methods:
  sftp   pure Go SFTP copy, supports password and key auth
  rsync  external rsync, requires local/remote rsync and usable SSH key
  auto   use rsync when available, otherwise fall back to SFTP

Logging:
  Logs are printed to screen by default. Use --log-file PATH to write a log file.
`)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", "config.yaml", "output config file")
	force := fs.Bool("force", false, "overwrite existing file")
	opts := bindCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyFlagOptions(fs, opts)
	logger.open(opts.LogFile, opts.Verbosity)

	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("配置文件 %s 已存在，如需覆盖请加 -force", *out)
	}
	if err := os.WriteFile(*out, []byte(defaultYAML()), 0644); err != nil {
		return fmt.Errorf("写入配置文件失败：%w", err)
	}
	logInfo("已生成默认配置：%s", *out)
	return nil
}

func runTrustAdd(args []string) error {
	cfg, err := loadCfg("trust-add", args)
	if err != nil {
		return err
	}
	key := expand(first(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	if err := ensureKey(key); err != nil {
		return err
	}
	line, marker, err := publicKeyLine(key)
	if err != nil {
		return err
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostPlan(hs)
	return summarize(batch(hs, cfg.Concurrency, func(h Host) Result {
		return result(h.Name, trustAdd(cfg, h, line, marker))
	}))
}

func runTrustRemove(args []string) error {
	cfg, err := loadCfg("trust-remove", args)
	if err != nil {
		return err
	}
	key := expand(first(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	line, marker, err := publicKeyLine(key)
	if err != nil {
		return err
	}
	prefix := keyPrefix(line)
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostPlan(hs)
	return summarize(batch(hs, cfg.Concurrency, func(h Host) Result {
		return result(h.Name, trustRemove(cfg, h, prefix, marker))
	}))
}

func runCopy(args []string) error {
	var src, remote, method string
	cfg, err := loadCfg("copy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&src, "src", "", "local file or directory")
		fs.StringVar(&remote, "remote-dir", "", "remote directory")
		fs.StringVar(&method, "copy-method", "", "sftp, rsync, or auto")
	})
	if err != nil {
		return err
	}
	overrideDeploy(&cfg, src, remote, "", "", method)
	if cfg.Deploy.SrcDir == "" || cfg.Deploy.RemoteDir == "" {
		return errors.New("copy 需要同时配置 src_dir 和 remote_dir，或使用 --src 与 --remote-dir")
	}
	if err := localOK(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostPlan(hs)
	return summarize(batch(hs, cfg.Concurrency, func(h Host) Result {
		return result(h.Name, copyOne(cfg, h))
	}))
}

func runExec(args []string) error {
	var cmd, mode string
	cfg, err := loadCfg("exec", args, func(fs *flag.FlagSet) {
		fs.StringVar(&cmd, "cmd", "", "remote command")
		fs.StringVar(&mode, "mode", "", "hidden or visible")
	})
	if err != nil {
		return err
	}
	overrideDeploy(&cfg, "", "", cmd, mode, "")
	if strings.TrimSpace(cfg.Deploy.Command) == "" {
		return errors.New("exec 需要远程命令，请使用 --cmd 或 deploy.command")
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostPlan(hs)
	return runByMode(cfg, hs,
		func(h Host) Result { return execHidden(cfg, h, cfg.Deploy.Command) },
		func(h Host) Result { return execVisible(cfg, h, cfg.Deploy.Command) },
	)
}

func runDeploy(args []string) error {
	var src, remote, cmd, mode, method string
	cfg, err := loadCfg("deploy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&src, "src", "", "local file or directory")
		fs.StringVar(&remote, "remote-dir", "", "remote directory")
		fs.StringVar(&cmd, "cmd", "", "remote command")
		fs.StringVar(&mode, "mode", "", "hidden or visible")
		fs.StringVar(&method, "copy-method", "", "sftp, rsync, or auto")
	})
	if err != nil {
		return err
	}
	overrideDeploy(&cfg, src, remote, cmd, mode, method)
	doCopy := cfg.Deploy.SrcDir != "" || cfg.Deploy.RemoteDir != ""
	doExec := strings.TrimSpace(cfg.Deploy.Command) != ""
	if !doCopy && !doExec {
		return errors.New("deploy 需要配置 src_dir+remote_dir、command，或二者都配置")
	}
	if doCopy {
		if cfg.Deploy.SrcDir == "" || cfg.Deploy.RemoteDir == "" {
			return errors.New("复制步骤需要同时配置 src_dir 和 remote_dir")
		}
		if err := localOK(cfg.Deploy.SrcDir); err != nil {
			return err
		}
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostPlan(hs)
	return runByMode(cfg, hs,
		func(h Host) Result { return deployOne(cfg, h, doCopy, doExec, false) },
		func(h Host) Result { return deployOne(cfg, h, doCopy, doExec, true) },
	)
}

func deployOne(cfg Config, h Host, doCopy, doExec, visible bool) Result {
	if doCopy {
		if err := copyOne(cfg, h); err != nil {
			return result(h.Name, err)
		}
	}
	if doExec {
		if visible {
			return execVisible(cfg, h, cfg.Deploy.Command)
		}
		return execHidden(cfg, h, cfg.Deploy.Command)
	}
	return result(h.Name, nil)
}

func bindCommonFlags(fs *flag.FlagSet) *Options {
	opts := &Options{}
	fs.StringVar(&opts.ConfigPath, "c", "config.yaml", "config path")
	fs.StringVar(&opts.ConfigPath, "config", "config.yaml", "config path")
	fs.StringVar(&opts.LogFile, "log-file", "", "log file path; empty means screen only")
	fs.BoolFunc("v", "verbose logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 1); return nil })
	fs.BoolFunc("vv", "more verbose logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 2); return nil })
	fs.BoolFunc("vvv", "trace logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 3); return nil })
	return opts
}

func applyFlagOptions(fs *flag.FlagSet, opts *Options) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "v":
			opts.Verbosity = max(opts.Verbosity, 1)
		case "vv":
			opts.Verbosity = max(opts.Verbosity, 2)
		case "vvv":
			opts.Verbosity = max(opts.Verbosity, 3)
		}
	})
}

type hook func(*flag.FlagSet)

func loadCfg(name string, args []string, hooks ...hook) (Config, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	opts := bindCommonFlags(fs)
	for _, h := range hooks {
		h(fs)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	applyFlagOptions(fs, opts)
	b, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败：%w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析 YAML 配置失败：%w", err)
	}
	defaults(&cfg)
	logFile := first(opts.LogFile, cfg.Logging.File)
	verbosity := max(opts.Verbosity, levelToVerbosity(cfg.Logging.Level))
	logger.open(logFile, verbosity)
	logDebug("config=%s log_file=%s verbosity=%d", opts.ConfigPath, logFile, verbosity)
	return cfg, nil
}

func defaults(c *Config) {
	if c.Concurrency <= 0 {
		c.Concurrency = 5
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Defaults.User == "" {
		c.Defaults.User = "root"
	}
	if c.Defaults.Port <= 0 {
		c.Defaults.Port = 22
	}
	if c.Defaults.Password == "" && c.Defaults.PasswordEnv == "" {
		c.Defaults.PasswordEnv = "SSHPASS"
	}
	if c.Trust.ManagedKey == "" {
		c.Trust.ManagedKey = "~/.ssh/deployctl_id_rsa"
	}
	if c.Deploy.Mode == "" {
		c.Deploy.Mode = "hidden"
	}
	if c.Deploy.CopyMethod == "" {
		c.Deploy.CopyMethod = "sftp"
	}
}

func overrideDeploy(c *Config, src, remote, cmd, mode, method string) {
	if src != "" {
		c.Deploy.SrcDir = src
	}
	if remote != "" {
		c.Deploy.RemoteDir = remote
	}
	if cmd != "" {
		c.Deploy.Command = cmd
	}
	if mode != "" {
		c.Deploy.Mode = mode
	}
	if method != "" {
		c.Deploy.CopyMethod = method
	}
}

func buildHosts(c Config) ([]Host, error) {
	if len(c.Hosts) == 0 {
		return nil, errors.New("配置中的 hosts 为空，请至少添加一台主机")
	}
	out := make([]Host, 0, len(c.Hosts))
	for _, n := range c.Hosts {
		name := strings.TrimSpace(n.Host)
		if name == "" {
			return nil, errors.New("hosts 中存在空的 host")
		}
		pass, passSource, passNote := resolvePassword(n, c.Defaults)
		if passNote != "" {
			logWarn("%s: %s", name, passNote)
		}
		key, keySource := resolveKey(n, c.Defaults)
		h := Host{Name: name, User: first(n.User, c.Defaults.User), Port: firstInt(n.Port, c.Defaults.Port), Password: pass, PasswordSource: passSource, Key: key, KeySource: keySource}
		if h.Password == "" && h.Key == "" {
			return nil, fmt.Errorf("%s 没有可用认证方式，请配置 password、password_env 或 key", h.Name)
		}
		out = append(out, h)
	}
	return out, nil
}

func resolvePassword(n Node, d Auth) (string, string, string) {
	if n.Password != "" {
		return n.Password, "host.password", ""
	}
	if d.Password != "" {
		return d.Password, "defaults.password", ""
	}
	if n.PasswordEnv != "" {
		v := os.Getenv(n.PasswordEnv)
		if v == "" {
			return "", "", fmt.Sprintf("环境变量 %q 为空", n.PasswordEnv)
		}
		return v, "env(" + n.PasswordEnv + ")", ""
	}
	if d.PasswordEnv != "" {
		v := os.Getenv(d.PasswordEnv)
		if v == "" {
			return "", "", fmt.Sprintf("默认密码环境变量 %q 为空", d.PasswordEnv)
		}
		return v, "env(" + d.PasswordEnv + ")", ""
	}
	return "", "", ""
}

func resolveKey(n Node, d Auth) (string, string) {
	if n.Key != "" {
		return n.Key, "host.key"
	}
	if d.Key != "" {
		return d.Key, "defaults.key"
	}
	return "", ""
}

func runByMode(cfg Config, hs []Host, hidden, visible func(Host) Result) error {
	mode := strings.ToLower(strings.TrimSpace(cfg.Deploy.Mode))
	if mode == "" {
		mode = "hidden"
	}
	switch mode {
	case "hidden":
		logInfo("执行模式：hidden，并发数：%d", cfg.Concurrency)
		return summarize(batch(hs, cfg.Concurrency, hidden))
	case "visible":
		logInfo("执行模式：visible，逐台执行")
		rs := make([]Result, 0, len(hs))
		for _, h := range hs {
			rs = append(rs, visible(h))
		}
		return summarize(rs)
	default:
		return fmt.Errorf("未知执行模式 %q，请使用 hidden 或 visible", mode)
	}
}

func copyOne(cfg Config, h Host) error {
	method := strings.ToLower(strings.TrimSpace(cfg.Deploy.CopyMethod))
	if method == "" {
		method = "sftp"
	}
	switch method {
	case "sftp":
		return copyViaSFTP(cfg, h)
	case "rsync":
		return copyViaRsync(cfg, h)
	case "auto":
		if rsyncUsable(cfg, h) {
			logInfo("%s: 使用 rsync 复制", h.Name)
			return copyViaRsync(cfg, h)
		}
		logInfo("%s: rsync 不可用，自动回退 SFTP", h.Name)
		return copyViaSFTP(cfg, h)
	default:
		return fmt.Errorf("未知复制方式 %q，请使用 sftp、rsync 或 auto", method)
	}
}

func copyViaSFTP(cfg Config, h Host) error {
	logInfo("%s: 连接中", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("SSH 连接失败：%w", err)
	}
	defer client.Close()
	logInfo("%s: SFTP 上传 %s -> %s", h.Name, cfg.Deploy.SrcDir, cfg.Deploy.RemoteDir)
	if err := uploadSFTP(client, cfg.Deploy.SrcDir, cfg.Deploy.RemoteDir); err != nil {
		return fmt.Errorf("SFTP 上传失败：%w", err)
	}
	logInfo("%s: 上传完成", h.Name)
	return nil
}

func copyViaRsync(cfg Config, h Host) error {
	if _, err := exec.LookPath("rsync"); err != nil {
		return errors.New("本机没有找到 rsync，请安装 rsync，或使用 --copy-method sftp")
	}
	key := usableKeyPath(h)
	if key == "" {
		return errors.New("rsync 需要可用 SSH 私钥；请先 trust-add 配置免密，或使用 --copy-method sftp/auto")
	}
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("rsync 前置 SSH 检查失败：%w", err)
	}
	_, code, err := runHidden(client, "command -v rsync >/dev/null 2>&1")
	_ = client.Close()
	if err != nil || code != 0 {
		return errors.New("远端没有找到 rsync，请安装 rsync，或使用 --copy-method sftp/auto")
	}
	sshCmd := fmt.Sprintf("ssh -p %d -i %s -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", h.Port, key)
	remote := fmt.Sprintf("%s@%s:%s/", h.User, h.Name, cfg.Deploy.RemoteDir)
	args := []string{"-az", "-e", sshCmd, filepath.Clean(cfg.Deploy.SrcDir), remote}
	logInfo("%s: rsync 上传 %s -> %s", h.Name, cfg.Deploy.SrcDir, cfg.Deploy.RemoteDir)
	cmd := exec.Command("rsync", args...)
	out, err := cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		logger.remoteOutput(h.Name, string(out), true)
	}
	if err != nil {
		return fmt.Errorf("rsync 执行失败：%w", err)
	}
	logInfo("%s: rsync 上传完成", h.Name)
	return nil
}

func rsyncUsable(cfg Config, h Host) bool {
	if _, err := exec.LookPath("rsync"); err != nil {
		logDebug("%s: 本机 rsync 不可用", h.Name)
		return false
	}
	if usableKeyPath(h) == "" {
		logDebug("%s: 没有可用于 rsync 的私钥", h.Name)
		return false
	}
	client, err := newSSHClient(cfg, h)
	if err != nil {
		logDebug("%s: rsync SSH 检查失败：%v", h.Name, err)
		return false
	}
	defer client.Close()
	_, code, err := runHidden(client, "command -v rsync >/dev/null 2>&1")
	return err == nil && code == 0
}

func execHidden(cfg Config, h Host, cmd string) Result {
	logInfo("%s: 执行命令", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return result(h.Name, fmt.Errorf("SSH 连接失败：%w", err))
	}
	defer client.Close()
	out, code, err := runHidden(client, cmd)
	if strings.TrimSpace(out) != "" {
		logger.remoteOutput(h.Name, out, true)
	}
	return Result{Host: h.Name, ExitCode: code, Err: err}
}

func execVisible(cfg Config, h Host, cmd string) Result {
	logInfo("%s: 实时执行命令", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return result(h.Name, fmt.Errorf("SSH 连接失败：%w", err))
	}
	defer client.Close()
	fmt.Printf("\n========== %s ==========\n", h.Name)
	logger.filePrintf("========== %s visible start ==========\n", h.Name)
	code, err := runVisible(client, cmd)
	fmt.Printf("========== %s exit=%d ==========\n\n", h.Name, code)
	logger.filePrintf("========== %s visible exit=%d ==========\n", h.Name, code)
	return Result{Host: h.Name, ExitCode: code, Err: err}
}

func newSSHClient(cfg Config, h Host) (*ssh.Client, error) {
	auth, methods, err := authMethods(h)
	if err != nil {
		return nil, err
	}
	logDebug("%s: user=%s port=%d auth=%s", h.Name, h.User, h.Port, strings.Join(methods, ","))
	conf := &ssh.ClientConfig{User: h.User, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: cfg.Timeout}
	client, err := ssh.Dial("tcp", net.JoinHostPort(h.Name, fmt.Sprintf("%d", h.Port)), conf)
	if err != nil {
		return nil, fmt.Errorf("认证或连接失败，请检查用户、端口、密码/私钥、sshd 配置；host=%s user=%s port=%d auth=%s", h.Name, h.User, h.Port, strings.Join(methods, ","))
	}
	return client, nil
}

func authMethods(h Host) ([]ssh.AuthMethod, []string, error) {
	methods := []ssh.AuthMethod{}
	names := []string{}
	if h.Key != "" {
		if signer, err := loadKey(expand(h.Key)); err != nil {
			logWarn("%s: 私钥不可用：%s", h.Name, h.Key)
		} else {
			methods = append(methods, ssh.PublicKeys(signer))
			names = append(names, "publickey:"+h.KeySource)
		}
	}
	if h.Password != "" {
		methods = append(methods, ssh.Password(h.Password), ssh.KeyboardInteractive(func(_ string, _ string, q []string, _ []bool) ([]string, error) {
			answers := make([]string, len(q))
			for i := range answers {
				answers[i] = h.Password
			}
			return answers, nil
		}))
		names = append(names, "password:"+h.PasswordSource, "keyboard-interactive:"+h.PasswordSource)
	}
	if len(methods) == 0 {
		return nil, nil, errors.New("没有可用认证方式，请配置 password/password_env 或 key")
	}
	return methods, names, nil
}

func trustAdd(cfg Config, h Host, publicLine, marker string) error {
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return err
	}
	defer client.Close()
	home, err := remoteHome(client)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	dir := path.Join(home, ".ssh")
	file := path.Join(dir, "authorized_keys")
	if err := sc.MkdirAll(dir); err != nil {
		return err
	}
	_ = sc.Chmod(dir, 0700)
	old, _ := readRemote(sc, file)
	prefix := keyPrefix(publicLine)
	if strings.Contains(old, marker) || hasKey(old, prefix) {
		logInfo("%s: 免密 key 已存在", h.Name)
		return nil
	}
	if old != "" && !strings.HasSuffix(old, "\n") {
		old += "\n"
	}
	if err := writeRemote(sc, file, old+publicLine, 0600); err != nil {
		return err
	}
	logInfo("%s: 已添加免密 key", h.Name)
	return nil
}

func trustRemove(cfg Config, h Host, prefix, marker string) error {
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return err
	}
	defer client.Close()
	home, err := remoteHome(client)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	file := path.Join(home, ".ssh", "authorized_keys")
	old, err := readRemote(sc, file)
	if err != nil {
		logInfo("%s: authorized_keys 不存在，跳过", h.Name)
		return nil
	}
	kept := []string{}
	changed := false
	for _, line := range strings.Split(old, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, marker) || strings.HasPrefix(trimmed, prefix) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		logInfo("%s: 未找到本工具管理的 key", h.Name)
		return nil
	}
	newContent := strings.Join(kept, "\n")
	if newContent != "" {
		newContent += "\n"
	}
	return writeRemote(sc, file, newContent, 0600)
}

func uploadSFTP(client *ssh.Client, src, remoteDir string) error {
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	dst := path.Join(remoteDir, filepath.Base(src))
	if !info.IsDir() {
		return uploadFile(sc, src, dst, info.Mode())
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return sc.MkdirAll(dst)
		}
		item, err := d.Info()
		if err != nil {
			return err
		}
		remotePath := path.Join(dst, filepath.ToSlash(rel))
		if d.IsDir() {
			return sc.MkdirAll(remotePath)
		}
		if !item.Mode().IsRegular() {
			logDebug("跳过非普通文件：%s", p)
			return nil
		}
		logTrace("upload %s -> %s", p, remotePath)
		return uploadFile(sc, p, remotePath, item.Mode())
	})
}

func uploadFile(sc *sftp.Client, src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := sc.MkdirAll(path.Dir(dst)); err != nil {
		return err
	}
	out, err := sc.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return sc.Chmod(dst, mode)
}

func runHidden(client *ssh.Client, cmd string) (string, int, error) {
	s, err := client.NewSession()
	if err != nil {
		return "", -1, err
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	return string(out), exitCode(err), err
}

func runVisible(client *ssh.Client, cmd string) (int, error) {
	s, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	s.Stdout = logger.visibleWriter(os.Stdout)
	s.Stderr = logger.visibleWriter(os.Stderr)
	s.Stdin = os.Stdin
	if err := s.Run(cmd); err != nil {
		return exitCode(err), err
	}
	return 0, nil
}

func remoteHome(client *ssh.Client) (string, error) {
	out, _, err := runHidden(client, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.Trim(strings.TrimSpace(out), `"`)
	if home == "" {
		return "", errors.New("远端 HOME 为空")
	}
	return home, nil
}

func ensureKey(p string) error {
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(p, pemBytes, 0600); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".pub", []byte(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))+"\n"), 0644); err != nil {
		return err
	}
	logInfo("已生成私钥：%s", p)
	return nil
}

func publicKeyLine(p string) (string, string, error) {
	signer, err := loadKey(p)
	if err != nil {
		return "", "", err
	}
	marker := markerPrefix + ":" + ssh.FingerprintSHA256(signer.PublicKey())
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + marker + "\n", marker, nil
}

func loadKey(p string) (ssh.Signer, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func readRemote(sc *sftp.Client, p string) (string, error) {
	f, err := sc.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

func writeRemote(sc *sftp.Client, p, content string, mode os.FileMode) error {
	f, err := sc.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, content); err != nil {
		return err
	}
	return sc.Chmod(p, mode)
}

func batch(hs []Host, n int, fn func(Host) Result) []Result {
	if n <= 0 {
		n = 1
	}
	jobs := make(chan Host)
	out := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				out <- fn(h)
			}
		}()
	}
	go func() {
		for _, h := range hs {
			jobs <- h
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()
	rs := []Result{}
	for r := range out {
		rs = append(rs, r)
	}
	return rs
}

func summarize(rs []Result) error {
	fail := 0
	fmt.Println("\n执行汇总：")
	logger.filePrintf("\n执行汇总：\n")
	for _, r := range rs {
		status := "成功"
		if r.Err != nil {
			status = "失败"
			fail++
		}
		line := fmt.Sprintf("  %-15s %-6s exit=%d", r.Host, status, r.ExitCode)
		if r.Err != nil {
			line += fmt.Sprintf(" err=%v", friendlyError(r.Err))
		}
		fmt.Println(line)
		logger.filePrintf(line + "\n")
	}
	if fail > 0 {
		return fmt.Errorf("有 %d 台主机执行失败", fail)
	}
	logInfo("全部主机执行成功")
	return nil
}

func result(host string, err error) Result { return Result{Host: host, ExitCode: exitCode(err), Err: err} }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ssh.ExitError
	if errors.As(err, &e) {
		return e.ExitStatus()
	}
	return -1
}

func localOK(p string) error {
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("本地路径不可访问：%s", p)
	}
	return nil
}

func usableKeyPath(h Host) string {
	if h.Key == "" {
		return ""
	}
	p := expand(h.Key)
	if _, err := loadKey(p); err == nil {
		return p
	}
	return ""
}

func logHostPlan(hs []Host) {
	for _, h := range hs {
		methods := []string{}
		if h.Key != "" {
			methods = append(methods, "publickey:"+h.KeySource)
		}
		if h.Password != "" {
			methods = append(methods, "password:"+h.PasswordSource)
		}
		logDebug("%s: user=%s port=%d auth=%s", h.Name, h.User, h.Port, strings.Join(methods, ","))
	}
}

func defaultYAML() string {
	return `concurrency: 5
timeout: 30s

logging:
  # 不配置 file 时只在屏幕显示日志；需要日志文件时取消下一行注释
  # file: "deployctl.log"
  level: "info"

defaults:
  user: root
  port: 22
  # password: "your-password"
  password_env: "SSHPASS"
  key: "~/.ssh/deployctl_id_rsa"

trust:
  managed_key: "~/.ssh/deployctl_id_rsa"

deploy:
  copy_method: sftp # sftp, rsync, auto
  src_dir: ""
  remote_dir: ""
  command: ""
  mode: hidden # hidden, visible

hosts:
  - host: 192.168.1.10
  - host: 192.168.1.11
`
}

func (l *Logger) open(file string, verbosity int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbosity = verbosity
	if strings.TrimSpace(file) == "" || file == "-" {
		return
	}
	if dir := filepath.Dir(file); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "警告：无法创建日志目录，日志文件已禁用：%v\n", err)
			return
		}
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：无法打开日志文件，日志文件已禁用：%v\n", err)
		return
	}
	l.file = f
	_, _ = fmt.Fprintf(l.file, "\n===== deployctl started at %s =====\n", time.Now().Format(time.RFC3339))
}

func (l *Logger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

func (l *Logger) log(level string, screenMin int, format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", level, fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = fmt.Fprintln(l.file, time.Now().Format(time.RFC3339), line)
	}
	if screenMin == 0 || l.verbosity >= screenMin {
		if level == "WARN" || level == "ERROR" {
			fmt.Fprintln(os.Stderr, line)
		} else {
			fmt.Println(line)
		}
	}
}

func (l *Logger) filePrintf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = fmt.Fprintf(l.file, format, args...)
	}
}

func (l *Logger) remoteOutput(host, out string, screen bool) {
	text := fmt.Sprintf("[REMOTE:%s]\n%s\n", host, strings.TrimRight(out, "\n"))
	l.filePrintf(text)
	if screen {
		fmt.Print(text)
	}
}

func (l *Logger) visibleWriter(w io.Writer) io.Writer {
	l.mu.Lock()
	f := l.file
	l.mu.Unlock()
	if f == nil {
		return w
	}
	return io.MultiWriter(w, f)
}

func friendlyError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "ssh: handshake failed: ", "")
	msg = strings.ReplaceAll(msg, "ssh: unable to authenticate", "SSH 认证失败")
	msg = strings.ReplaceAll(msg, "no supported methods remain", "没有可用认证方式")
	return errors.New(msg)
}

func logInfo(f string, a ...any)  { logger.log("INFO", 0, f, a...) }
func logWarn(f string, a ...any)  { logger.log("WARN", 0, f, a...) }
func logErr(f string, a ...any)   { logger.log("ERROR", 0, f, a...) }
func logDebug(f string, a ...any) { logger.log("DEBUG", 1, f, a...) }
func logTrace(f string, a ...any) { logger.log("TRACE", 3, f, a...) }

func levelToVerbosity(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return 1
	case "trace":
		return 3
	default:
		return 0
	}
}

func keyPrefix(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return strings.TrimSpace(line)
	}
	return fields[0] + " " + fields[1]
}

func hasKey(content, prefix string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

func expand(p string) string {
	if p == "~" {
		h, _ := os.UserHomeDir()
		return h
	}
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

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
	Logging     LoggingConfig `yaml:"logging"`
	Defaults    Auth          `yaml:"defaults"`
	Trust       Trust         `yaml:"trust"`
	Deploy      Deploy        `yaml:"deploy"`
	Hosts       []Node        `yaml:"hosts"`
}

type LoggingConfig struct {
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
	SrcDir    string `yaml:"src_dir"`
	RemoteDir string `yaml:"remote_dir"`
	Command   string `yaml:"command"`
	Mode      string `yaml:"mode"`
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
		err = fmt.Errorf("unknown command: %s", os.Args[1])
	}

	if err != nil {
		logErr("%v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`deployctl - batch SSH/SFTP deployment tool

Usage:
  deployctl init -o config.yaml [-force]
  deployctl trust-add -c config.yaml [-v|-vv|-vvv] [--log-file deployctl.log]
  deployctl trust-remove -c config.yaml [-v|-vv|-vvv] [--log-file deployctl.log]
  deployctl copy -c config.yaml --src local-package --remote-dir /opt [-v|-vv|-vvv]
  deployctl exec -c config.yaml --cmd "hostname && uptime" --mode hidden|visible [-v|-vv|-vvv]
  deployctl deploy -c config.yaml --mode hidden|visible [-v|-vv|-vvv]

Modes:
  hidden   concurrent execution, collect output, show summary and exit code
  visible  sequential execution, stream remote output directly, Ctrl+C can stop

Logging:
  Screen shows normal logs by default. Use -v, -vv, or -vvv for more detail.
  Logs are also written to logging.file in config or --log-file.
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
	logger.open(first(opts.LogFile, "deployctl.log"), opts.Verbosity)
	defer logger.close()

	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s exists, use -force to overwrite", *out)
	}
	if err := os.WriteFile(*out, []byte(defaultYAML()), 0644); err != nil {
		return err
	}
	logInfo("created default config: %s", *out)
	logInfo("edit hosts, defaults, deploy, and logging sections before running other commands")
	return nil
}

func runTrustAdd(args []string) error {
	cfg, err := loadCfg("trust-add", args)
	if err != nil {
		return err
	}
	defer logger.close()

	key := expand(def(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	if err := ensureKey(key); err != nil {
		return err
	}
	line, mark, err := pubLine(key)
	if err != nil {
		return err
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostAuthPlan(hs)
	return summary(batch(hs, cfg.Concurrency, func(h Host) Result { return result(h.Name, trustAdd(cfg, h, line, mark)) }))
}

func runTrustRemove(args []string) error {
	cfg, err := loadCfg("trust-remove", args)
	if err != nil {
		return err
	}
	defer logger.close()

	key := expand(def(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	line, mark, err := pubLine(key)
	if err != nil {
		return err
	}
	pre := keyPrefix(line)
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostAuthPlan(hs)
	return summary(batch(hs, cfg.Concurrency, func(h Host) Result { return result(h.Name, trustRemove(cfg, h, pre, mark)) }))
}

func runCopy(args []string) error {
	var src, remote string
	cfg, err := loadCfg("copy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&src, "src", "", "local file or directory")
		fs.StringVar(&remote, "remote-dir", "", "remote directory")
	})
	if err != nil {
		return err
	}
	defer logger.close()

	overrideDeploy(&cfg, src, remote, "", "")
	if err := localOK(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostAuthPlan(hs)
	return summary(batch(hs, cfg.Concurrency, func(h Host) Result { return result(h.Name, copyOne(cfg, h)) }))
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
	defer logger.close()

	if strings.TrimSpace(cmd) == "" {
		return errors.New("missing --cmd")
	}
	overrideDeploy(&cfg, "", "", cmd, mode)
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostAuthPlan(hs)
	return runByMode(cfg, hs, func(h Host) Result { return execHidden(cfg, h, cmd) }, func(h Host) Result { return execVisible(cfg, h, cmd) })
}

func runDeploy(args []string) error {
	var src, remote, cmd, mode string
	cfg, err := loadCfg("deploy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&src, "src", "", "local file or directory")
		fs.StringVar(&remote, "remote-dir", "", "remote directory")
		fs.StringVar(&cmd, "cmd", "", "remote command after copy")
		fs.StringVar(&mode, "mode", "", "hidden or visible")
	})
	if err != nil {
		return err
	}
	defer logger.close()

	overrideDeploy(&cfg, src, remote, cmd, mode)
	if err := localOK(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hs, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	logHostAuthPlan(hs)
	return runByMode(cfg, hs,
		func(h Host) Result {
			if e := copyOne(cfg, h); e != nil {
				return result(h.Name, e)
			}
			return execHidden(cfg, h, cfg.Deploy.Command)
		},
		func(h Host) Result {
			if e := copyOne(cfg, h); e != nil {
				return result(h.Name, e)
			}
			return execVisible(cfg, h, cfg.Deploy.Command)
		},
	)
}

func runByMode(cfg Config, hs []Host, hidden, visible func(Host) Result) error {
	mode := strings.ToLower(strings.TrimSpace(cfg.Deploy.Mode))
	if mode == "" {
		mode = "hidden"
	}
	switch mode {
	case "hidden":
		logInfo("run mode: hidden, concurrency=%d", cfg.Concurrency)
		return summary(batch(hs, cfg.Concurrency, hidden))
	case "visible":
		logInfo("run mode: visible, sequential execution")
		rs := make([]Result, 0, len(hs))
		for _, h := range hs {
			rs = append(rs, visible(h))
		}
		return summary(rs)
	default:
		return fmt.Errorf("invalid mode %q, expected hidden or visible", mode)
	}
}

type hook func(*flag.FlagSet)

func bindCommonFlags(fs *flag.FlagSet) *Options {
	opts := &Options{}
	fs.StringVar(&opts.ConfigPath, "c", "config.yaml", "config path")
	fs.StringVar(&opts.ConfigPath, "config", "config.yaml", "config path")
	fs.StringVar(&opts.LogFile, "log-file", "", "log file path")
	fs.BoolFunc("v", "verbose logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 1); return nil })
	fs.BoolFunc("vv", "more verbose logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 2); return nil })
	fs.BoolFunc("vvv", "debug-level verbose logs", func(string) error { opts.Verbosity = max(opts.Verbosity, 3); return nil })
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
		return Config{}, fmt.Errorf("read config failed: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml failed: %w", err)
	}
	defaults(&cfg)

	logFile := first(opts.LogFile, cfg.Logging.File, "deployctl.log")
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
	if c.Logging.File == "" {
		c.Logging.File = "deployctl.log"
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
	if c.Deploy.SrcDir == "" {
		c.Deploy.SrcDir = "local-package"
	}
	if c.Deploy.RemoteDir == "" {
		c.Deploy.RemoteDir = "/opt"
	}
	if c.Deploy.Command == "" {
		c.Deploy.Command = fmt.Sprintf("cd %s && chmod +x install.sh && ./install.sh", quote(path.Join(c.Deploy.RemoteDir, filepath.Base(c.Deploy.SrcDir))))
	}
	if c.Deploy.Mode == "" {
		c.Deploy.Mode = "hidden"
	}
}

func overrideDeploy(c *Config, src, remote, cmd, mode string) {
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
}

func buildHosts(c Config) ([]Host, error) {
	if len(c.Hosts) == 0 {
		return nil, errors.New("config hosts is empty")
	}

	out := make([]Host, 0, len(c.Hosts))
	for _, n := range c.Hosts {
		name := strings.TrimSpace(n.Host)
		if name == "" {
			return nil, errors.New("host can not be empty")
		}

		password, passwordSource, passwordNote := resolvePassword(n, c.Defaults)
		if passwordNote != "" {
			logWarn("%s: %s", name, passwordNote)
		}

		key, keySource := resolveKey(n, c.Defaults)
		h := Host{
			Name:           name,
			User:           first(n.User, c.Defaults.User),
			Port:           firstInt(n.Port, c.Defaults.Port),
			Password:       password,
			PasswordSource: passwordSource,
			Key:            key,
			KeySource:      keySource,
		}
		if h.Password == "" && h.Key == "" {
			return nil, fmt.Errorf("%s has no password or key; configure host/defaults password, password_env, or key", h.Name)
		}
		out = append(out, h)
	}
	return out, nil
}

func resolvePassword(n Node, d Auth) (password, source, note string) {
	if n.Password != "" {
		return n.Password, "host.password", ""
	}
	if d.Password != "" {
		return d.Password, "defaults.password", ""
	}
	if n.PasswordEnv != "" {
		v := os.Getenv(n.PasswordEnv)
		if v == "" {
			return "", "", fmt.Sprintf("password_env %q is set on host but environment variable is empty", n.PasswordEnv)
		}
		return v, "env(" + n.PasswordEnv + ")", ""
	}
	if d.PasswordEnv != "" {
		v := os.Getenv(d.PasswordEnv)
		if v == "" {
			return "", "", fmt.Sprintf("defaults.password_env %q is set but environment variable is empty", d.PasswordEnv)
		}
		return v, "env(" + d.PasswordEnv + ")", ""
	}
	return "", "", ""
}

func resolveKey(n Node, d Auth) (key, source string) {
	if n.Key != "" {
		return n.Key, "host.key"
	}
	if d.Key != "" {
		return d.Key, "defaults.key"
	}
	return "", ""
}

func logHostAuthPlan(hs []Host) {
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

func localOK(p string) error {
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("local path not accessible: %w", err)
	}
	return nil
}

func copyOne(c Config, h Host) error {
	logInfo("%s: connecting", h.Name)
	cl, err := sshClient(c, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer cl.Close()
	logInfo("%s: upload %s -> %s", h.Name, c.Deploy.SrcDir, c.Deploy.RemoteDir)
	if err := upload(cl, c.Deploy.SrcDir, c.Deploy.RemoteDir); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	logInfo("%s: upload done", h.Name)
	return nil
}

func execHidden(c Config, h Host, cmd string) Result {
	logInfo("%s: executing hidden", h.Name)
	cl, err := sshClient(c, h)
	if err != nil {
		return result(h.Name, fmt.Errorf("ssh connect failed: %w", err))
	}
	defer cl.Close()
	out, code, err := runHidden(cl, cmd)
	if strings.TrimSpace(out) != "" {
		logger.remoteOutput(h.Name, out, true)
	}
	return Result{Host: h.Name, ExitCode: code, Err: err}
}

func execVisible(c Config, h Host, cmd string) Result {
	logInfo("%s: executing visible", h.Name)
	cl, err := sshClient(c, h)
	if err != nil {
		return result(h.Name, fmt.Errorf("ssh connect failed: %w", err))
	}
	defer cl.Close()
	fmt.Printf("\n========== %s ==========\n", h.Name)
	logger.filePrintf("========== %s visible start =========="+"\n", h.Name)
	code, err := runVisible(cl, cmd)
	fmt.Printf("========== %s exit=%d ==========\n\n", h.Name, code)
	logger.filePrintf("========== %s visible exit=%d =========="+"\n", h.Name, code)
	return Result{Host: h.Name, ExitCode: code, Err: err}
}

func trustAdd(c Config, h Host, line, mark string) error {
	cl, err := sshClient(c, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer cl.Close()
	home, err := home(cl)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(cl)
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
	pre := keyPrefix(line)
	if strings.Contains(old, mark) || hasKey(old, pre) {
		logInfo("%s: trust key already exists", h.Name)
		return nil
	}
	if old != "" && !strings.HasSuffix(old, "\n") {
		old += "\n"
	}
	if err := writeRemote(sc, file, old+line, 0600); err != nil {
		return err
	}
	logInfo("%s: trust key added", h.Name)
	return nil
}

func trustRemove(c Config, h Host, pre, mark string) error {
	cl, err := sshClient(c, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer cl.Close()
	home, err := home(cl)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(cl)
	if err != nil {
		return err
	}
	defer sc.Close()
	file := path.Join(home, ".ssh", "authorized_keys")
	old, err := readRemote(sc, file)
	if err != nil {
		logInfo("%s: authorized_keys not found, skip", h.Name)
		return nil
	}
	keep := []string{}
	changed := false
	for _, l := range strings.Split(old, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.Contains(t, mark) || strings.HasPrefix(t, pre) {
			changed = true
			continue
		}
		keep = append(keep, l)
	}
	if !changed {
		logInfo("%s: managed key not found", h.Name)
		return nil
	}
	newc := strings.Join(keep, "\n")
	if newc != "" {
		newc += "\n"
	}
	return writeRemote(sc, file, newc, 0600)
}

func sshClient(c Config, h Host) (*ssh.Client, error) {
	auth, methods, err := auths(h)
	if err != nil {
		return nil, err
	}
	logDebug("%s: connecting as %s:%d using %s", h.Name, h.User, h.Port, strings.Join(methods, ","))
	conf := &ssh.ClientConfig{User: h.User, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: c.Timeout}
	cl, err := ssh.Dial("tcp", net.JoinHostPort(h.Name, fmt.Sprintf("%d", h.Port)), conf)
	if err != nil {
		return nil, fmt.Errorf("%w; user=%s port=%d auth=%s", err, h.User, h.Port, strings.Join(methods, ","))
	}
	return cl, nil
}

func auths(h Host) ([]ssh.AuthMethod, []string, error) {
	var a []ssh.AuthMethod
	var methods []string

	if h.Key != "" {
		signer, err := loadKey(expand(h.Key))
		if err != nil {
			logWarn("%s: key %q from %s is not usable: %v", h.Name, h.Key, h.KeySource, err)
		} else {
			a = append(a, ssh.PublicKeys(signer))
			methods = append(methods, "publickey:"+h.KeySource)
		}
	}
	if h.Password != "" {
		a = append(a, ssh.Password(h.Password), ssh.KeyboardInteractive(func(_ string, _ string, q []string, _ []bool) ([]string, error) {
			ans := make([]string, len(q))
			for i := range ans {
				ans[i] = h.Password
			}
			return ans, nil
		}))
		methods = append(methods, "password:"+h.PasswordSource, "keyboard-interactive:"+h.PasswordSource)
	}
	if len(a) == 0 {
		return nil, nil, errors.New("no auth method available; configure password/password_env or key")
	}
	return a, methods, nil
}

func upload(cl *ssh.Client, src, remoteDir string) error {
	sc, err := sftp.NewClient(cl)
	if err != nil {
		return err
	}
	defer sc.Close()
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	dst := path.Join(remoteDir, filepath.Base(src))
	if !st.IsDir() {
		logDebug("upload file %s -> %s", src, dst)
		return upFile(sc, src, dst, st.Mode())
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return sc.MkdirAll(dst)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rp := path.Join(dst, filepath.ToSlash(rel))
		if d.IsDir() {
			logTrace("mkdir %s", rp)
			return sc.MkdirAll(rp)
		}
		if !info.Mode().IsRegular() {
			logDebug("skip non-regular file: %s", p)
			return nil
		}
		logTrace("upload file %s -> %s", p, rp)
		return upFile(sc, p, rp, info.Mode())
	})
}

func upFile(sc *sftp.Client, src, dst string, mode os.FileMode) error {
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

func runHidden(cl *ssh.Client, cmd string) (string, int, error) {
	s, err := cl.NewSession()
	if err != nil {
		return "", -1, err
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	return string(out), exitCode(err), err
}

func runVisible(cl *ssh.Client, cmd string) (int, error) {
	s, err := cl.NewSession()
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

func run(cl *ssh.Client, cmd string) (string, error) {
	out, _, err := runHidden(cl, cmd)
	return out, err
}

func home(cl *ssh.Client) (string, error) {
	out, err := run(cl, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(out)
	if h == "" {
		return "", errors.New("remote HOME is empty")
	}
	return strings.Trim(h, `"`), nil
}

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

func ensureKey(p string) error {
	if _, err := os.Stat(p); err == nil {
		logDebug("reuse existing key: %s", p)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(p, pemBytes, 0600); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".pub", []byte(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))+"\n"), 0644); err != nil {
		return err
	}
	logInfo("generated key: %s", p)
	return nil
}

func pubLine(p string) (string, string, error) {
	s, err := loadKey(p)
	if err != nil {
		return "", "", err
	}
	mark := markerPrefix + ":" + ssh.FingerprintSHA256(s.PublicKey())
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " " + mark + "\n", mark, nil
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

func writeRemote(sc *sftp.Client, p, s string, m os.FileMode) error {
	f, err := sc.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, s); err != nil {
		return err
	}
	return sc.Chmod(p, m)
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

func summary(rs []Result) error {
	fail := 0
	msg := "\nSummary:"
	fmt.Println(msg)
	logger.filePrintf(msg + "\n")
	for _, r := range rs {
		st := "OK"
		if r.Err != nil {
			st = "FAILED"
			fail++
		}
		line := fmt.Sprintf("  %-15s %-7s exit=%d", r.Host, st, r.ExitCode)
		if r.Err != nil {
			line += fmt.Sprintf(" err=%v", r.Err)
		}
		fmt.Println(line)
		logger.filePrintf(line + "\n")
	}
	if fail > 0 {
		return fmt.Errorf("completed with %d failed host(s)", fail)
	}
	logInfo("all hosts completed successfully")
	return nil
}

func result(host string, err error) Result { return Result{Host: host, ExitCode: exitCode(err), Err: err} }

func defaultYAML() string {
	return `concurrency: 5
timeout: 30s

logging:
  file: "deployctl.log"
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
  src_dir: "local-package"
  remote_dir: "/opt"
  command: "cd /opt/local-package && chmod +x install.sh && ./install.sh"
  mode: hidden

hosts:
  - host: 192.168.1.10
  - host: 192.168.1.11
  - host: 192.168.1.12
`
}

func (l *Logger) open(file string, verbosity int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbosity = verbosity
	if file == "" || file == "-" {
		return
	}
	dir := filepath.Dir(file)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] create log dir failed: %v\n", err)
			return
		}
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] open log file failed: %v\n", err)
		return
	}
	l.file = f
	l.filePrintf("\n===== deployctl started at %s =====\n", time.Now().Format(time.RFC3339))
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
	if l.verbosity >= screenMin || screenMin == 0 {
		if level == "ERROR" || level == "WARN" {
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

func keyPrefix(l string) string {
	f := strings.Fields(l)
	if len(f) < 2 {
		return strings.TrimSpace(l)
	}
	return f[0] + " " + f[1]
}
func hasKey(s, pre string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), pre) {
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
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func first(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}
func firstInt(v ...int) int {
	for _, x := range v {
		if x > 0 {
			return x
		}
	}
	return 0
}
func def(v, d string) string {
	if v != "" {
		return v
	}
	return d
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

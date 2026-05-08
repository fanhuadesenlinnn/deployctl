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
	Defaults    HostDefaults  `yaml:"defaults"`
	Trust       TrustConfig   `yaml:"trust"`
	Deploy      DeployConfig  `yaml:"deploy"`
	Hosts       []HostConfig  `yaml:"hosts"`
}

type HostDefaults struct {
	User        string `yaml:"user"`
	Port        int    `yaml:"port"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	Key         string `yaml:"key"`
}

type HostConfig struct {
	Host        string `yaml:"host"`
	User        string `yaml:"user"`
	Port        int    `yaml:"port"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	Key         string `yaml:"key"`
}

type TrustConfig struct {
	ManagedKey string `yaml:"managed_key"`
}

type DeployConfig struct {
	SrcDir    string `yaml:"src_dir"`
	RemoteDir string `yaml:"remote_dir"`
	Command   string `yaml:"command"`
}

type Host struct {
	Name     string
	User     string
	Port     int
	Password string
	Key      string
}

type Result struct {
	Host string
	Err  error
}

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
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "copy":
		err = runCopy(os.Args[2:])
	case "exec":
		err = runExec(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command: %s", os.Args[1])
	}

	if err != nil {
		logError("%v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`deployctl - batch SSH/SFTP deployment tool

Usage:
  deployctl init          -o config.yaml
  deployctl trust-add     -c config.yaml
  deployctl trust-remove  -c config.yaml
  deployctl deploy        -c config.yaml
  deployctl copy          -c config.yaml --src AnyBackupClient --remote-dir /opt
  deployctl exec          -c config.yaml --cmd "hostname && uptime"

Examples:
  deployctl init -o config.yaml
  export SSHPASS='password'
  deployctl trust-add -c config.yaml
  deployctl deploy -c config.yaml
`)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", "config.yaml", "output config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists, refuse to overwrite", *out)
	}
	if err := os.WriteFile(*out, []byte(defaultConfigYAML()), 0644); err != nil {
		return err
	}
	logInfo("created default config: %s", *out)
	return nil
}

func runTrustAdd(args []string) error {
	cfg, err := loadConfigFromFlags("trust-add", args)
	if err != nil {
		return err
	}
	key := expandPath(defaultString(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	if err := ensureKeyPair(key); err != nil {
		return err
	}
	pub, marker, err := publicKeyLineFromPrivateKey(key)
	if err != nil {
		return err
	}
	hosts, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	return summarize(runBatch(hosts, cfg.Concurrency, func(h Host) error {
		return trustAddOne(cfg, h, pub, marker)
	}))
}

func runTrustRemove(args []string) error {
	cfg, err := loadConfigFromFlags("trust-remove", args)
	if err != nil {
		return err
	}
	key := expandPath(defaultString(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	pub, marker, err := publicKeyLineFromPrivateKey(key)
	if err != nil {
		return err
	}
	prefix := publicKeyPrefix(pub)
	hosts, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	return summarize(runBatch(hosts, cfg.Concurrency, func(h Host) error {
		return trustRemoveOne(cfg, h, prefix, marker)
	}))
}

func runDeploy(args []string) error {
	cfg, err := loadConfigFromFlags("deploy", args)
	if err != nil {
		return err
	}
	if err := validateDir(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hosts, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	return summarize(runBatch(hosts, cfg.Concurrency, func(h Host) error {
		if err := copyOne(cfg, h); err != nil {
			return err
		}
		return execOne(cfg, h, cfg.Deploy.Command)
	}))
}

func runCopy(args []string) error {
	var src, remoteDir string
	cfg, err := loadConfigFromFlags("copy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&src, "src", "", "local source directory")
		fs.StringVar(&remoteDir, "remote-dir", "", "remote target directory")
	})
	if err != nil {
		return err
	}
	if src != "" {
		cfg.Deploy.SrcDir = src
	}
	if remoteDir != "" {
		cfg.Deploy.RemoteDir = remoteDir
	}
	if err := validateDir(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hosts, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	return summarize(runBatch(hosts, cfg.Concurrency, func(h Host) error { return copyOne(cfg, h) }))
}

func runExec(args []string) error {
	var cmd string
	cfg, err := loadConfigFromFlags("exec", args, func(fs *flag.FlagSet) {
		fs.StringVar(&cmd, "cmd", "", "remote command")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(cmd) == "" {
		return errors.New("missing --cmd")
	}
	hosts, err := buildHosts(cfg)
	if err != nil {
		return err
	}
	return summarize(runBatch(hosts, cfg.Concurrency, func(h Host) error { return execOne(cfg, h, cmd) }))
}

type flagHook func(*flag.FlagSet)

func loadConfigFromFlags(name string, args []string, hooks ...flagHook) (Config, error) {
	var configPath string
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&configPath, "c", "config.yaml", "config yaml path")
	fs.StringVar(&configPath, "config", "config.yaml", "config yaml path")
	for _, hook := range hooks {
		hook(fs)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return Config{}, err
	}
	applyDefaults(&cfg)
	return cfg, nil
}

func loadConfig(file string) (Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Config{}, fmt.Errorf("read config failed: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml failed: %w", err)
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 5
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Defaults.User == "" {
		cfg.Defaults.User = "root"
	}
	if cfg.Defaults.Port <= 0 {
		cfg.Defaults.Port = 22
	}
	if cfg.Defaults.PasswordEnv == "" && cfg.Defaults.Password == "" {
		cfg.Defaults.PasswordEnv = "SSHPASS"
	}
	if cfg.Trust.ManagedKey == "" {
		cfg.Trust.ManagedKey = "~/.ssh/deployctl_id_rsa"
	}
	if cfg.Deploy.SrcDir == "" {
		cfg.Deploy.SrcDir = "AnyBackupClient"
	}
	if cfg.Deploy.RemoteDir == "" {
		cfg.Deploy.RemoteDir = "/opt"
	}
	if cfg.Deploy.Command == "" {
		cfg.Deploy.Command = fmt.Sprintf("cd %s && chmod +x install-silent.sh && ./install-silent.sh", shellQuote(path.Join(cfg.Deploy.RemoteDir, filepath.Base(cfg.Deploy.SrcDir))))
	}
}

func buildHosts(cfg Config) ([]Host, error) {
	if len(cfg.Hosts) == 0 {
		return nil, errors.New("config hosts is empty")
	}
	var hosts []Host
	for _, raw := range cfg.Hosts {
		name := strings.TrimSpace(raw.Host)
		if name == "" {
			return nil, errors.New("host can not be empty")
		}
		password := firstNonEmpty(raw.Password, cfg.Defaults.Password)
		passwordEnv := firstNonEmpty(raw.PasswordEnv, cfg.Defaults.PasswordEnv)
		if password == "" && passwordEnv != "" {
			password = os.Getenv(passwordEnv)
		}
		h := Host{
			Name:     name,
			User:     firstNonEmpty(raw.User, cfg.Defaults.User),
			Port:     firstPositive(raw.Port, cfg.Defaults.Port),
			Password: password,
			Key:      firstNonEmpty(raw.Key, cfg.Defaults.Key),
		}
		if h.Password == "" && h.Key == "" {
			return nil, fmt.Errorf("%s has no password or key", h.Name)
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

func validateDir(src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("source directory not accessible: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}
	return nil
}

func copyOne(cfg Config, h Host) error {
	logInfo("%s: connecting", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	logInfo("%s: upload %s -> %s", h.Name, cfg.Deploy.SrcDir, cfg.Deploy.RemoteDir)
	if err := uploadDirectory(client, cfg.Deploy.SrcDir, cfg.Deploy.RemoteDir); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	logInfo("%s: upload done", h.Name)
	return nil
}

func execOne(cfg Config, h Host, cmd string) error {
	logInfo("%s: executing command", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	out, err := runRemoteCommand(client, cmd)
	if strings.TrimSpace(out) != "" {
		fmt.Printf("[REMOTE:%s]\n%s\n", h.Name, strings.TrimRight(out, "\n"))
	}
	if err != nil {
		return fmt.Errorf("remote command failed: %w", err)
	}
	logInfo("%s: command done", h.Name)
	return nil
}

func trustAddOne(cfg Config, h Host, publicLine, marker string) error {
	logInfo("%s: adding trust key", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	home, err := getRemoteHome(client)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	sshDir := path.Join(home, ".ssh")
	authFile := path.Join(sshDir, "authorized_keys")
	if err := sc.MkdirAll(sshDir); err != nil {
		return err
	}
	_ = sc.Chmod(sshDir, 0700)
	content, _ := readRemoteFile(sc, authFile)
	prefix := publicKeyPrefix(publicLine)
	if strings.Contains(content, marker) || authorizedKeysContainsPrefix(content, prefix) {
		logInfo("%s: trust key already exists", h.Name)
		return nil
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += publicLine
	if err := writeRemoteFile(sc, authFile, content, 0600); err != nil {
		return err
	}
	logInfo("%s: trust key added", h.Name)
	return nil
}

func trustRemoveOne(cfg Config, h Host, keyPrefix, marker string) error {
	logInfo("%s: removing trust key", h.Name)
	client, err := newSSHClient(cfg, h)
	if err != nil {
		return fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()
	home, err := getRemoteHome(client)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	authFile := path.Join(home, ".ssh", "authorized_keys")
	content, err := readRemoteFile(sc, authFile)
	if err != nil {
		logInfo("%s: authorized_keys not found, skip", h.Name)
		return nil
	}
	var kept []string
	changed := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, marker) || strings.HasPrefix(trimmed, keyPrefix) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		logInfo("%s: managed key not found", h.Name)
		return nil
	}
	newContent := strings.Join(kept, "\n")
	if newContent != "" {
		newContent += "\n"
	}
	if err := writeRemoteFile(sc, authFile, newContent, 0600); err != nil {
		return err
	}
	logInfo("%s: trust key removed", h.Name)
	return nil
}

func newSSHClient(cfg Config, h Host) (*ssh.Client, error) {
	auth, err := buildAuthMethods(h)
	if err != nil {
		return nil, err
	}
	sshConfig := &ssh.ClientConfig{User: h.User, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: cfg.Timeout}
	return ssh.Dial("tcp", net.JoinHostPort(h.Name, fmt.Sprintf("%d", h.Port)), sshConfig)
}

func buildAuthMethods(h Host) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if h.Key != "" {
		if signer, err := loadPrivateKey(expandPath(h.Key)); err == nil {
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}
	if h.Password != "" {
		methods = append(methods, ssh.Password(h.Password))
		methods = append(methods, ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = h.Password
			}
			return answers, nil
		}))
	}
	if len(methods) == 0 {
		return nil, errors.New("no auth method available")
	}
	return methods, nil
}

func uploadDirectory(client *ssh.Client, localDir, remoteBaseDir string) error {
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	localDir = filepath.Clean(localDir)
	remoteRoot := path.Join(remoteBaseDir, filepath.Base(localDir))
	if err := sc.MkdirAll(remoteRoot); err != nil {
		return err
	}
	return filepath.WalkDir(localDir, func(localPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		remotePath := path.Join(remoteRoot, filepath.ToSlash(rel))
		if entry.IsDir() {
			return sc.MkdirAll(remotePath)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return uploadFile(sc, localPath, remotePath, info.Mode())
	})
}

func uploadFile(sc *sftp.Client, localPath, remotePath string, mode os.FileMode) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := sc.MkdirAll(path.Dir(remotePath)); err != nil {
		return err
	}
	dst, err := sc.OpenFile(remotePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return sc.Chmod(remotePath, mode)
}

func runRemoteCommand(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

func getRemoteHome(client *ssh.Client) (string, error) {
	out, err := runRemoteCommand(client, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", errors.New("remote HOME is empty")
	}
	return home, nil
}

func ensureKeyPair(privateKeyPath string) error {
	if _, err := os.Stat(privateKeyPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(privateKeyPath), 0700); err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(privateKeyPath, privatePEM, 0600); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	publicLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + "\n"
	if err := os.WriteFile(privateKeyPath+".pub", []byte(publicLine), 0644); err != nil {
		return err
	}
	logInfo("generated key: %s", privateKeyPath)
	return nil
}

func publicKeyLineFromPrivateKey(privateKeyPath string) (string, string, error) {
	signer, err := loadPrivateKey(privateKeyPath)
	if err != nil {
		return "", "", err
	}
	marker := markerPrefix + ":" + ssh.FingerprintSHA256(signer.PublicKey())
	publicLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + marker + "\n"
	return publicLine, marker, nil
}

func loadPrivateKey(file string) (ssh.Signer, error) {
	key, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(key)
}

func readRemoteFile(sc *sftp.Client, remotePath string) (string, error) {
	f, err := sc.Open(remotePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeRemoteFile(sc *sftp.Client, remotePath, content string, mode os.FileMode) error {
	f, err := sc.OpenFile(remotePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, content); err != nil {
		return err
	}
	return sc.Chmod(remotePath, mode)
}

func runBatch(hosts []Host, concurrency int, fn func(Host) error) []Result {
	if concurrency <= 0 {
		concurrency = 1
	}
	jobs := make(chan Host)
	results := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				results <- Result{Host: h.Name, Err: fn(h)}
			}
		}()
	}
	go func() {
		for _, h := range hosts {
			jobs <- h
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	var all []Result
	for r := range results {
		if r.Err != nil {
			logError("%s: %v", r.Host, r.Err)
		}
		all = append(all, r)
	}
	return all
}

func summarize(results []Result) error {
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("completed with %d failed host(s)", failed)
	}
	logInfo("all hosts completed successfully")
	return nil
}

func defaultConfigYAML() string {
	return `concurrency: 5
timeout: 30s

defaults:
  user: root
  port: 22
  password_env: "SSHPASS"
  key: "~/.ssh/deployctl_id_rsa"

trust:
  managed_key: "~/.ssh/deployctl_id_rsa"

deploy:
  src_dir: "AnyBackupClient"
  remote_dir: "/opt"
  command: "cd /opt/AnyBackupClient && chmod +x install-silent.sh && ./install-silent.sh"

hosts:
  - host: 10.71.43.6
  - host: 10.71.43.7
  - host: 10.71.43.8
`
}

func publicKeyPrefix(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return strings.TrimSpace(line)
	}
	return fields[0] + " " + fields[1]
}

func authorizedKeysContainsPrefix(content, prefix string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

func expandPath(p string) string {
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func firstNonEmpty(values ...string) string { for _, v := range values { if v != "" { return v } }; return "" }
func firstPositive(values ...int) int { for _, v := range values { if v > 0 { return v } }; return 0 }
func defaultString(value, fallback string) string { if value != "" { return value }; return fallback }
func logInfo(format string, args ...any) { fmt.Printf("[INFO] "+format+"\n", args...) }
func logError(format string, args ...any) { fmt.Fprintf(os.Stderr, "[ERROR] "+format+"\n", args...) }

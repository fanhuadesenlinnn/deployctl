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
	Defaults    Auth          `yaml:"defaults"`
	Trust       Trust         `yaml:"trust"`
	Deploy      Deploy        `yaml:"deploy"`
	Hosts       []Node        `yaml:"hosts"`
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

type Trust struct{ ManagedKey string `yaml:"managed_key"` }
type Deploy struct {
	SrcDir    string `yaml:"src_dir"`
	RemoteDir string `yaml:"remote_dir"`
	Command   string `yaml:"command"`
	Mode      string `yaml:"mode"`
}
type Host struct {
	Name, User, Password, Key string
	Port                      int
}
type Result struct {
	Host     string
	ExitCode int
	Err      error
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
	fmt.Println(`deployctl - batch SSH/SFTP deployment tool

Usage:
  deployctl init -o config.yaml [-force]
  deployctl trust-add -c config.yaml
  deployctl trust-remove -c config.yaml
  deployctl copy -c config.yaml --src AnyBackupClient --remote-dir /opt
  deployctl exec -c config.yaml --cmd "hostname && uptime" --mode hidden|visible
  deployctl deploy -c config.yaml --mode hidden|visible

Modes:
  hidden   concurrent execution, collect output, show summary and exit code
  visible  sequential execution, stream remote output directly, Ctrl+C can stop
`)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", "config.yaml", "output config file")
	force := fs.Bool("force", false, "overwrite existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s exists, use -force to overwrite", *out)
	}
	if err := os.WriteFile(*out, []byte(defaultYAML()), 0644); err != nil {
		return err
	}
	logInfo("created default config: %s", *out)
	return nil
}
func runTrustAdd(args []string) error {
	cfg, err := loadCfg("trust-add", args)
	if err != nil {
		return err
	}
	key := expand(def(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	if err := ensureKey(key); err != nil {
		return err
	}
	line, mark, err := pubLine(key)
	if err != nil {
		return err
	}
	hs, err := hosts(cfg)
	if err != nil {
		return err
	}
	return summary(batch(hs, cfg.Concurrency, func(h Host) Result { return result(h.Name, trustAdd(cfg, h, line, mark)) }))
}
func runTrustRemove(args []string) error {
	cfg, err := loadCfg("trust-remove", args)
	if err != nil {
		return err
	}
	key := expand(def(cfg.Trust.ManagedKey, "~/.ssh/deployctl_id_rsa"))
	line, mark, err := pubLine(key)
	if err != nil {
		return err
	}
	pre := keyPrefix(line)
	hs, err := hosts(cfg)
	if err != nil {
		return err
	}
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
	over(&cfg, src, remote, "", "")
	if err := localOK(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hs, err := hosts(cfg)
	if err != nil {
		return err
	}
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
	if strings.TrimSpace(cmd) == "" {
		return errors.New("missing --cmd")
	}
	over(&cfg, "", "", cmd, mode)
	hs, err := hosts(cfg)
	if err != nil {
		return err
	}
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
	over(&cfg, src, remote, cmd, mode)
	if err := localOK(cfg.Deploy.SrcDir); err != nil {
		return err
	}
	hs, err := hosts(cfg)
	if err != nil {
		return err
	}
	return runByMode(cfg, hs, func(h Host) Result {
		if e := copyOne(cfg, h); e != nil {
			return result(h.Name, e)
		}
		return execHidden(cfg, h, cfg.Deploy.Command)
	}, func(h Host) Result {
		if e := copyOne(cfg, h); e != nil {
			return result(h.Name, e)
		}
		return execVisible(cfg, h, cfg.Deploy.Command)
	})
}

func runByMode(cfg Config, hs []Host, hidden, visible func(Host) Result) error {
	m := strings.ToLower(strings.TrimSpace(cfg.Deploy.Mode))
	if m == "" {
		m = "hidden"
	}
	switch m {
	case "hidden":
		return summary(batch(hs, cfg.Concurrency, hidden))
	case "visible":
		rs := make([]Result, 0, len(hs))
		for _, h := range hs {
			rs = append(rs, visible(h))
		}
		return summary(rs)
	default:
		return fmt.Errorf("invalid mode %q", m)
	}
}

type hook func(*flag.FlagSet)

func loadCfg(name string, args []string, hooks ...hook) (Config, error) {
	var p string
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&p, "c", "config.yaml", "config path")
	fs.StringVar(&p, "config", "config.yaml", "config path")
	for _, h := range hooks {
		h(fs)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Config{}, fmt.Errorf("read config failed: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml failed: %w", err)
	}
	defaults(&cfg)
	return cfg, nil
}
func defaults(c *Config) {
	if c.Concurrency <= 0 {
		c.Concurrency = 5
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
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
		c.Deploy.SrcDir = "AnyBackupClient"
	}
	if c.Deploy.RemoteDir == "" {
		c.Deploy.RemoteDir = "/opt"
	}
	if c.Deploy.Command == "" {
		c.Deploy.Command = fmt.Sprintf("cd %s && chmod +x install-silent.sh && ./install-silent.sh", quote(path.Join(c.Deploy.RemoteDir, filepath.Base(c.Deploy.SrcDir))))
	}
	if c.Deploy.Mode == "" {
		c.Deploy.Mode = "hidden"
	}
}
func over(c *Config, src, remote, cmd, mode string) {
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
func hosts(c Config) ([]Host, error) {
	if len(c.Hosts) == 0 {
		return nil, errors.New("config hosts is empty")
	}
	out := make([]Host, 0, len(c.Hosts))
	for _, n := range c.Hosts {
		name := strings.TrimSpace(n.Host)
		if name == "" {
			return nil, errors.New("host can not be empty")
		}
		pass := first(n.Password, c.Defaults.Password)
		env := first(n.PasswordEnv, c.Defaults.PasswordEnv)
		if pass == "" && env != "" {
			pass = os.Getenv(env)
		}
		h := Host{Name: name, User: first(n.User, c.Defaults.User), Port: firstInt(n.Port, c.Defaults.Port), Password: pass, Key: first(n.Key, c.Defaults.Key)}
		if h.Password == "" && h.Key == "" {
			return nil, fmt.Errorf("%s has no password or key", h.Name)
		}
		out = append(out, h)
	}
	return out, nil
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
		fmt.Printf("[REMOTE:%s]\n%s\n", h.Name, strings.TrimRight(out, "\n"))
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
	code, err := runVisible(cl, cmd)
	fmt.Printf("========== %s exit=%d ==========\n\n", h.Name, code)
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
	auth, err := auths(h)
	if err != nil {
		return nil, err
	}
	conf := &ssh.ClientConfig{User: h.User, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: c.Timeout}
	return ssh.Dial("tcp", net.JoinHostPort(h.Name, fmt.Sprintf("%d", h.Port)), conf)
}
func auths(h Host) ([]ssh.AuthMethod, error) {
	var a []ssh.AuthMethod
	if h.Key != "" {
		if s, err := loadKey(expand(h.Key)); err == nil {
			a = append(a, ssh.PublicKeys(s))
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
	}
	if len(a) == 0 {
		return nil, errors.New("no auth method available")
	}
	return a, nil
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
			return sc.MkdirAll(rp)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
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
	s.Stdout = os.Stdout
	s.Stderr = os.Stderr
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
	fmt.Println("\nSummary:")
	for _, r := range rs {
		st := "OK"
		if r.Err != nil {
			st = "FAILED"
			fail++
		}
		fmt.Printf("  %-15s %-7s exit=%d", r.Host, st, r.ExitCode)
		if r.Err != nil {
			fmt.Printf(" err=%v", r.Err)
		}
		fmt.Println()
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
  mode: hidden

hosts:
  - host: 10.71.43.6
  - host: 10.71.43.7
  - host: 10.71.43.8
`}
func keyPrefix(l string) string { f := strings.Fields(l); if len(f)<2 { return strings.TrimSpace(l) }; return f[0]+" "+f[1] }
func hasKey(s, pre string) bool { for _, l := range strings.Split(s,"\n") { if strings.HasPrefix(strings.TrimSpace(l), pre) { return true } }; return false }
func expand(p string) string { if p=="~" { h,_:=os.UserHomeDir(); return h }; if strings.HasPrefix(p,"~/") { h,_:=os.UserHomeDir(); return filepath.Join(h,p[2:]) }; return p }
func quote(s string) string { return "'"+strings.ReplaceAll(s,"'",`'\''`)+"'" }
func first(v ...string) string { for _,x := range v { if x!="" { return x } }; return "" }
func firstInt(v ...int) int { for _,x := range v { if x>0 { return x } }; return 0 }
func def(v,d string) string { if v!="" { return v }; return d }
func logInfo(f string,a ...any){ fmt.Printf("[INFO] "+f+"\n",a...) }
func logErr(f string,a ...any){ fmt.Fprintf(os.Stderr,"[ERROR] "+f+"\n",a...) }

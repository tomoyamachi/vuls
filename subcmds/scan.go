package subcmds

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/asaskevich/govalidator"
	c "github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/logging"
	"github.com/future-architect/vuls/scanner"
	"github.com/google/subcommands"
	"github.com/k0kubun/pp"
)

// ScanCmd is Subcommand of host discovery mode
type ScanCmd struct {
	configPath     string
	askKeyPassword bool
	timeoutSec     int
	scanTimeoutSec int
	cacheDBPath    string
}

// Name return subcommand name
func (*ScanCmd) Name() string { return "scan" }

// Synopsis return synopsis
func (*ScanCmd) Synopsis() string { return "Scan vulnerabilities" }

// Usage return usage
func (*ScanCmd) Usage() string {
	return `scan:
	scan
		[-config=/path/to/config.toml]
		[-results-dir=/path/to/results]
		[-log-dir=/path/to/log]
		[-cachedb-path=/path/to/cache.db]
		[-http-proxy=http://192.168.0.1:8080]
		[-ask-key-password]
		[-timeout=300]
		[-timeout-scan=7200]
		[-debug]
		[-quiet]
		[-pipe]
		[-vvv]
		[-ips]


		[SERVER]...
`
}

// SetFlags set flag
func (p *ScanCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.Conf.Debug, "debug", false, "debug mode")
	f.BoolVar(&c.Conf.Quiet, "quiet", false, "Quiet mode. No output on stdout")

	wd, _ := os.Getwd()
	defaultConfPath := filepath.Join(wd, "config.toml")
	f.StringVar(&p.configPath, "config", defaultConfPath, "/path/to/toml")

	defaultResultsDir := filepath.Join(wd, "results")
	f.StringVar(&c.Conf.ResultsDir, "results-dir", defaultResultsDir, "/path/to/results")

	defaultLogDir := logging.GetDefaultLogDir()
	f.StringVar(&c.Conf.LogDir, "log-dir", defaultLogDir, "/path/to/log")

	defaultCacheDBPath := filepath.Join(wd, "cache.db")
	f.StringVar(&p.cacheDBPath, "cachedb-path", defaultCacheDBPath,
		"/path/to/cache.db (local cache of changelog for Ubuntu/Debian)")

	f.StringVar(&c.Conf.HTTPProxy, "http-proxy", "",
		"http://proxy-url:port (default: empty)")

	f.BoolVar(&p.askKeyPassword, "ask-key-password", false,
		"Ask ssh privatekey password before scanning",
	)

	f.BoolVar(&c.Conf.Pipe, "pipe", false, "Use stdin via PIPE")

	f.BoolVar(&c.Conf.DetectIPS, "ips", false, "retrieve IPS information")
	f.BoolVar(&c.Conf.Vvv, "vvv", false, "ssh -vvv")

	f.IntVar(&p.timeoutSec, "timeout", 5*60,
		"Number of seconds for processing other than scan",
	)

	f.IntVar(&p.scanTimeoutSec, "timeout-scan", 120*60,
		"Number of seconds for scanning vulnerabilities for all servers",
	)
}

// Execute execute
func (p *ScanCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	logging.Log = logging.NewCustomLogger(c.Conf.Debug, c.Conf.Quiet, c.Conf.LogDir, "", "")
	logging.Log.Infof("vuls-%s-%s", c.Version, c.Revision)

	if err := mkdirDotVuls(); err != nil {
		logging.Log.Errorf("Failed to create $HOME/.vuls err: %+v", err)
		return subcommands.ExitUsageError
	}

	if len(p.cacheDBPath) != 0 {
		if ok, _ := govalidator.IsFilePath(p.cacheDBPath); !ok {
			logging.Log.Errorf("Cache DB path must be a *Absolute* file path. -cache-dbpath: %s",
				p.cacheDBPath)
			return subcommands.ExitUsageError
		}
	}

	var keyPass string
	var err error
	if p.askKeyPassword {
		prompt := "SSH key password: "
		if keyPass, err = getPasswd(prompt); err != nil {
			logging.Log.Error(err)
			return subcommands.ExitFailure
		}
	}

	err = c.Load(p.configPath, keyPass)
	if err != nil {
		msg := []string{
			fmt.Sprintf("Error loading %s", p.configPath),
			"If you update Vuls and get this error, there may be incompatible changes in config.toml",
			"Please check config.toml template : https://vuls.io/docs/en/usage-settings.html",
		}
		logging.Log.Errorf("%s\n%+v", strings.Join(msg, "\n"), err)
		return subcommands.ExitUsageError
	}

	logging.Log.Info("Start scanning")
	logging.Log.Infof("config: %s", p.configPath)

	var servernames []string
	if 0 < len(f.Args()) {
		servernames = f.Args()
	} else if c.Conf.Pipe {
		bytes, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			logging.Log.Errorf("Failed to read stdin. err: %+v", err)
			return subcommands.ExitFailure
		}
		fields := strings.Fields(string(bytes))
		if 0 < len(fields) {
			servernames = fields
		}
	}

	targets := make(map[string]c.ServerInfo)
	for _, arg := range servernames {
		found := false
		for servername, info := range c.Conf.Servers {
			if servername == arg {
				targets[servername] = info
				found = true
				break
			}
		}
		if !found {
			logging.Log.Errorf("%s is not in config", arg)
			return subcommands.ExitUsageError
		}
	}
	if 0 < len(servernames) {
		// if scan target servers are specified by args, set to the config
		c.Conf.Servers = targets
	} else {
		// if not specified by args, scan all servers in the config
		targets = c.Conf.Servers
	}
	logging.Log.Debugf("%s", pp.Sprintf("%v", targets))

	logging.Log.Info("Validating config...")
	if !c.Conf.ValidateOnScan() {
		return subcommands.ExitUsageError
	}

	s := scanner.Scanner{
		TimeoutSec:     p.timeoutSec,
		ScanTimeoutSec: p.scanTimeoutSec,
		CacheDBPath:    p.cacheDBPath,
		Targets:        targets,
		Debug:          c.Conf.Debug,
		Quiet:          c.Conf.Quiet,
		LogDir:         c.Conf.LogDir,
	}

	if err := s.Scan(); err != nil {
		logging.Log.Errorf("Failed to scan: %+v", err)
		return subcommands.ExitFailure
	}

	fmt.Printf("\n\n\n")
	fmt.Println("To view the detail, vuls tui is useful.")
	fmt.Println("To send a report, run vuls report -h.")

	return subcommands.ExitSuccess
}

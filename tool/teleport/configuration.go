/*
Copyright 2015-16 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/config"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
)

// CommandLineFlags stores command line flag values, it's a much simplified subset
// of Teleport configuration (which is fully expressed via YAML config file)
type CommandLineFlags struct {
	// --name flag
	NodeName string
	// --auth-server flag
	AuthServerAddr string
	// --token flag
	AuthToken string
	// --listen-ip flag
	ListenIP net.IP
	// --advertise-ip flag
	AdvertiseIP net.IP
	// --config flag
	ConfigFile string
	// --roles flag
	Roles string
	// -d flag
	Debug bool
	// --labels flag
	Labels string
}

// readConfigFile reads /etc/teleport.yaml (or whatever is passed via --config flag)
// and overrides values in 'cfg' structure
func readConfigFile(cliConfigPath string) (*config.FileConfig, error) {
	configFilePath := defaults.ConfigFilePath
	// --config tells us to use a specific conf. file:
	if cliConfigPath != "" {
		configFilePath = cliConfigPath
		if !fileExists(configFilePath) {
			return nil, trace.Errorf("file not found: %s", configFilePath)
		}
	}
	// default config doesn't exist? quietly return:
	if !fileExists(configFilePath) {
		log.Info("not using a config file")
		return nil, nil
	}
	log.Debug("reading config file: ", configFilePath)
	return config.ReadFromFile(configFilePath)
}

// applyFileConfig applies confniguration from a YAML file to Teleport
// runtime config
func applyFileConfig(fc *config.FileConfig, cfg *service.Config) error {
	// no config file? no problem
	if fc == nil {
		return nil
	}
	// merge file-based config with defaults in 'cfg'
	if fc.Auth.Disabled() {
		cfg.Auth.Enabled = false
	}
	if fc.SSH.Disabled() {
		cfg.SSH.Enabled = false
	}
	if fc.Proxy.Disabled() {
		cfg.Proxy.Enabled = false
	}
	applyString(fc.NodeName, &cfg.Hostname)

	// apply "advertise_ip" setting:
	advertiseIP := fc.AdvertiseIP
	if advertiseIP != nil {
		if err := validateAdvertiseIP(advertiseIP); err != nil {
			return trace.Wrap(err)
		}
		cfg.AdvertiseIP = advertiseIP
	}

	// config file has auth servers in there?
	if len(fc.AuthServers) > 0 {
		cfg.AuthServers = make(service.NetAddrSlice, 0, len(fc.AuthServers))
		for _, as := range fc.AuthServers {
			addr, err := utils.ParseAddr(as)
			if err != nil {
				return trace.Errorf("cannot parse auth server address: '%v'", as)
			}
			cfg.AuthServers = append(cfg.AuthServers, *addr)
		}
	}
	cfg.ApplyToken(fc.AuthToken)
	cfg.Auth.DomainName = fc.Auth.DomainName

	// configure storage:
	switch fc.Storage.Type {
	case teleport.BoltBackendType:
		cfg.ConfigureBolt(fc.Storage.DirName)
	case teleport.ETCDBackendType:
		if err := cfg.ConfigureETCD(fc.Storage.DirName, fc.Storage.Peers, fc.Storage.Prefix); err != nil {
			return trace.Wrap(err)
		}
	case "":
		break // not set
	default:
		return trace.Wrap(teleport.BadParameter(
			"storage", fmt.Sprintf("unsupported storage type: '%v'", fc.Storage.Type)))
	}

	// apply logger settings
	switch fc.Logger.Output {
	case "":
		break // not set
	case "stderr", "error", "2":
		log.SetOutput(os.Stderr)
	case "stdout", "out", "1":
		log.SetOutput(os.Stdout)
	default:
		// assume it's a file path:
		logFile, err := os.Create(fc.Logger.Output)
		if err != nil {
			return trace.Wrap(err, "failed to create the log file")
		}
		log.SetOutput(logFile)
	}
	switch strings.ToLower(fc.Logger.Severity) {
	case "":
		break // not set
	case "info":
		log.SetLevel(log.InfoLevel)
	case "err", "error":
		log.SetLevel(log.ErrorLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	default:
		return trace.Errorf("unsupported logger severity: '%v'", fc.Logger.Severity)
	}
	// apply connection throttling:
	limiters := []limiter.LimiterConfig{
		cfg.SSH.Limiter,
		cfg.Auth.Limiter,
		cfg.Proxy.Limiter,
	}
	for _, l := range limiters {
		if fc.Limits.MaxConnections > 0 {
			l.MaxConnections = fc.Limits.MaxConnections
		}
		if fc.Limits.MaxUsers > 0 {
			l.MaxNumberOfUsers = fc.Limits.MaxUsers
		}
		for _, rate := range fc.Limits.Rates {
			l.Rates = append(l.Rates, limiter.Rate{
				Period:  rate.Period,
				Average: rate.Average,
				Burst:   rate.Burst,
			})
		}
	}
	// apply "proxy_service" section
	if fc.Proxy.ListenAddress != "" {
		addr, err := utils.ParseHostPortAddr(fc.Proxy.ListenAddress, int(defaults.SSHProxyListenPort))
		if err != nil {
			return trace.Wrap(err)
		}
		cfg.Proxy.SSHAddr = *addr
	}
	if fc.Proxy.WebAddr != "" {
		addr, err := utils.ParseHostPortAddr(fc.Proxy.WebAddr, int(defaults.HTTPListenPort))
		if err != nil {
			return trace.Wrap(err)
		}
		cfg.Proxy.WebAddr = *addr
	}
	if fc.Proxy.KeyFile != "" {
		if !fileExists(fc.Proxy.KeyFile) {
			return trace.Errorf("https key does not exist: %s", fc.Proxy.KeyFile)
		}
		cfg.Proxy.TLSKey = fc.Proxy.KeyFile
	}
	if fc.Proxy.CertFile != "" {
		if !fileExists(fc.Proxy.CertFile) {
			return trace.Errorf("https cert does not exist: %s", fc.Proxy.CertFile)
		}
		cfg.Proxy.TLSCert = fc.Proxy.CertFile
	}

	// apply "auth_service" section
	if fc.Auth.ListenAddress != "" {
		addr, err := utils.ParseHostPortAddr(fc.Auth.ListenAddress, int(defaults.AuthListenPort))
		if err != nil {
			return trace.Wrap(err)
		}
		cfg.Auth.SSHAddr = *addr
	}

	// apply "ssh_service" section
	if fc.SSH.ListenAddress != "" {
		addr, err := utils.ParseHostPortAddr(fc.SSH.ListenAddress, int(defaults.SSHServerListenPort))
		if err != nil {
			return trace.Wrap(err)
		}
		cfg.SSH.Addr = *addr
	}
	if fc.SSH.Labels != nil {
		cfg.SSH.Labels = make(map[string]string)
		for k, v := range fc.SSH.Labels {
			cfg.SSH.Labels[k] = v
		}
	}
	if fc.SSH.Commands != nil {
		cfg.SSH.CmdLabels = make(services.CommandLabels)
		for _, cmdLabel := range fc.SSH.Commands {
			cfg.SSH.CmdLabels[cmdLabel.Name] = services.CommandLabel{
				Period:  cmdLabel.Period,
				Command: cmdLabel.Command,
				Result:  "",
			}
		}
	}
	return nil
}

// applyString takes 'src' and overwrites target with it, unless 'src' is empty
// returns 'True' if 'src' was not empty
func applyString(src string, target *string) bool {
	if src != "" {
		*target = src
		return true
	}
	return false
}

// configure merges command line arguments with what's in a configuration file
// with CLI commands taking precedence
func configure(clf *CommandLineFlags) (cfg *service.Config, err error) {
	// create the default configuration:
	cfg = service.MakeDefaultConfig()

	// load /etc/teleport.yaml and apply it's values:
	fileConf, err := readConfigFile(clf.ConfigFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err = applyFileConfig(fileConf, cfg); err != nil {
		return nil, trace.Wrap(err)
	}
	// apply --debug flag:
	if clf.Debug {
		cfg.Console = ioutil.Discard
		utils.InitLoggerDebug()
	}

	// apply --roles flag:
	if clf.Roles != "" {
		if err := validateRoles(clf.Roles); err != nil {
			return cfg, trace.Wrap(err)
		}
		cfg.SSH.Enabled = strings.Index(clf.Roles, defaults.RoleNode) != -1
		cfg.Auth.Enabled = strings.Index(clf.Roles, defaults.RoleAuthService) != -1
		cfg.Proxy.Enabled = strings.Index(clf.Roles, defaults.RoleProxy) != -1
		cfg.ReverseTunnel.Enabled = cfg.Proxy.Enabled
	}

	// apply --auth-server flag:
	if clf.AuthServerAddr != "" {
		if cfg.Auth.Enabled {
			log.Warnf("not starting the local auth service. --auth-server flag tells to connect to another auth server")
			cfg.Auth.Enabled = false
		}
		addr, err := utils.ParseHostPortAddr(clf.AuthServerAddr, int(defaults.AuthListenPort))
		if err != nil {
			return cfg, trace.Wrap(err)
		}
		log.Infof("Using auth server: %v", addr.FullAddress())
		cfg.AuthServers = []utils.NetAddr{*addr}
	}

	// apply --name flag:
	if clf.NodeName != "" {
		cfg.Hostname = clf.NodeName
	}

	// apply --token flag:
	cfg.ApplyToken(clf.AuthToken)

	// apply --listen-ip flag:
	if clf.ListenIP != nil {
		applyListenIP(clf.ListenIP, cfg)
	}

	// --advertise-ip flag
	if clf.AdvertiseIP != nil {
		if err := validateAdvertiseIP(clf.AdvertiseIP); err != nil {
			return nil, trace.Wrap(err)
		}
		cfg.AdvertiseIP = clf.AdvertiseIP
	}

	// apply --labels flag
	if err = parseLabels(clf.Labels, &cfg.SSH); err != nil {
		return nil, trace.Wrap(err)
	}

	// locate web assets if web proxy is enabled
	if cfg.Proxy.Enabled {
		cfg.Proxy.AssetsDir, err = locateWebAssets()
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	return cfg, nil
}

// parseLabels takes the value of --labels flag and tries to correctly populate
// sshConf.Labels and sshConf.CmdLabels
func parseLabels(spec string, sshConf *service.SSHConfig) error {
	if spec == "" {
		return nil
	}
	// base syntax parsing, the spec must be in the form of 'key=value,more="better"`
	lmap, err := client.ParseLabelSpec(spec)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(lmap) > 0 {
		sshConf.CmdLabels = make(services.CommandLabels, 0)
		sshConf.Labels = make(map[string]string, 0)
	}
	// see which labels are actually command labels:
	for key, value := range lmap {
		cmdLabel, err := isCmdLabelSpec(value)
		if err != nil {
			return trace.Wrap(err)
		}
		if cmdLabel != nil {
			sshConf.CmdLabels[key] = *cmdLabel
		} else {
			sshConf.Labels[key] = value
		}
	}
	return nil
}

// isCmdLabelSpec tries to interpret a given string as a "command label" spec.
// A command label spec looks like [time_duration:command param1 param2 ...] where
// time_duration is in "1h2m1s" form.
//
// Example of a valid spec: "[1h:/bin/uname -m]"
func isCmdLabelSpec(spec string) (*services.CommandLabel, error) {
	// command spec? (surrounded by brackets?)
	if len(spec) > 5 && spec[0] == '[' && spec[len(spec)-1] == ']' {
		invalidSpecError := teleport.BadParameter("label",
			fmt.Sprintf("invalid command label spec: '%s'", spec))
		spec = strings.Trim(spec, "[]")
		idx := strings.IndexRune(spec, ':')
		if idx < 0 {
			return nil, trace.Wrap(invalidSpecError)
		}
		periodSpec := spec[:idx]
		period, err := time.ParseDuration(periodSpec)
		if err != nil {
			return nil, trace.Wrap(invalidSpecError)
		}
		cmdSpec := spec[idx+1:]
		if len(cmdSpec) < 1 {
			return nil, trace.Wrap(invalidSpecError)
		}
		var openQuote bool = false
		return &services.CommandLabel{
			Period: period,
			Command: strings.FieldsFunc(cmdSpec, func(c rune) bool {
				if c == '"' {
					openQuote = !openQuote
				}
				return unicode.IsSpace(c) && !openQuote
			}),
		}, nil
	}
	// not a valid spec
	return nil, nil
}

// applyListenIP replaces all 'listen addr' settings for all services with
// a given IP
func applyListenIP(ip net.IP, cfg *service.Config) {
	listeningAddresses := []*utils.NetAddr{
		&cfg.Auth.SSHAddr,
		&cfg.Auth.SSHAddr,
		&cfg.Proxy.SSHAddr,
		&cfg.Proxy.WebAddr,
		&cfg.SSH.Addr,
		&cfg.Proxy.ReverseTunnelListenAddr,
	}
	for _, addr := range listeningAddresses {
		replaceHost(addr, ip.String())
	}
}

// replaceHost takes utils.NetAddr and replaces the hostname in it, preserving
// the original port
func replaceHost(addr *utils.NetAddr, newHost string) {
	_, port, err := net.SplitHostPort(addr.Addr)
	if err != nil {
		log.Errorf("failed parsing address: '%v'", addr.Addr)
	}
	addr.Addr = net.JoinHostPort(newHost, port)
}

func fileExists(fp string) bool {
	_, err := os.Stat(fp)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

// validateRoles makes sure that value upassed to --roles flag is valid
func validateRoles(roles string) error {
	for _, role := range strings.Split(roles, ",") {
		switch role {
		case defaults.RoleAuthService,
			defaults.RoleNode,
			defaults.RoleProxy:
			break
		default:
			return trace.Errorf("unknown role: '%s'", role)
		}
	}
	return nil
}

func validateAdvertiseIP(advertiseIP net.IP) error {
	if advertiseIP.IsLoopback() || advertiseIP.IsUnspecified() || advertiseIP.IsMulticast() {
		return teleport.BadParameter("advertise-ip", fmt.Sprintf("unreachable advertise IP: %v", advertiseIP))
	}
	return nil
}

// DirsToLookForWebAssets defines the locations where teleport proxy looks for
// its web assets
var DirsToLookForWebAssets = []string{
	"/usr/local/share/teleport",
	"/usr/share/teleport",
	"/opt/teleport",
}

// locates the web assets required for the Proxy to start. Retursn the full path
// to web assets directory
func locateWebAssets() (string, error) {
	const errorMessage = "Cannot determine location of web assets."
	assetsToCheck := []string{
		"index.html",
		"/app/app.js",
	}
	// checker function to determine if dirPath contains the web assets
	locateAssets := func(dirPath string) bool {
		for _, af := range assetsToCheck {
			if !fileExists(filepath.Join(dirPath, af)) {
				return false
			}
		}
		return true
	}
	// check the directory where teleport binary is located first:
	exeDir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return "", trace.Wrap(err)
	}
	if locateAssets(exeDir) {
		return exeDir, nil
	}
	// look in other possible locations:
	for _, dir := range DirsToLookForWebAssets {
		if locateAssets(dir) {
			return dir, nil
		}
	}
	return "",
		trace.Errorf("Cannot find web assets. Unable to locate %v", filepath.Join(exeDir, assetsToCheck[0]))
}

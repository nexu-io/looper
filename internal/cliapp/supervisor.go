package cliapp

import "github.com/nexu-io/looper/internal/config"

type Supervisor interface {
	Install(config config.Config, binaryPath, logDir string) error
	Uninstall() error
	Start() error
	Stop() error
	ConfigType() config.DaemonMode
	Label() string
}

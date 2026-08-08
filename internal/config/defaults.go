package config

import "time"

const (
	DefaultGitHubURL      = "https://github.com"
	DefaultRunnerGroup    = "default"
	DefaultDockerHost     = "unix:///var/run/docker.sock"
	DefaultPullPolicy     = PullPolicyIfNotPresent
	DefaultHealthListen   = "127.0.0.1:8080"
	DefaultShutdownPolicy = ShutdownPolicyLeave
	DefaultLogFormat      = LogFormatJSON
	DefaultLogLevel       = LogLevelInfo
)

const (
	DefaultProvisioningTimeout = 5 * time.Minute
	DefaultStopTimeout         = 30 * time.Second
	DefaultShutdownGrace       = 2 * time.Minute
)

// Package logger provides logging configuration utilities for the gitlab-shell
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	v2log "gitlab.com/gitlab-org/labkit/v2/log"

	"gitlab.com/gitlab-org/gitlab-shell/v14/internal/config"
)

// ConfigureLogger uses text output if either log_format or GITLAB_LOG_FORMAT is "text".
// Labkit reads GITLAB_LOG_FORMAT directly. Passing the config's "json" default
// would override that environment setting, so only "text" is passed through.
// See the omnibus logging documentation:
// https://docs.gitlab.com/omnibus/settings/logs/#json-logging
func ConfigureLogger(cfg *config.Config) io.Closer {
	logConfig := &v2log.Config{}
	if strings.EqualFold(cfg.LogFormat, string(v2log.FormatText)) {
		logConfig.Format = v2log.FormatText
	}
	if cfg.LogLevel != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(cfg.LogLevel)); err == nil {
			logConfig.LogLevel = &level
		}
	}
	logger, logCloser, err := v2log.NewWithFile(cfg.LogFile, logConfig)
	slog.SetDefault(logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure log file %q: %v\n", cfg.LogFile, err)
		return nil
	}

	return logCloser
}

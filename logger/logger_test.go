package logger

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/gitlab-org/gitlab-shell/v14/internal/config"
)

func createTempFile(t *testing.T) string {
	t.Helper()

	tmpFile, err := os.CreateTemp(t.TempDir(), "logtest-")
	require.NoError(t, err)
	tmpFile.Close()

	return tmpFile.Name()
}

func TestConfigureLabkitV2Log(t *testing.T) {
	const (
		jsonFormat   = "json"
		expectedText = `msg="text format test"`
		expectedJSON = `"msg":"text format test"`
	)

	for _, tc := range []struct {
		name         string
		logFormat    string
		envLogFormat string
		expected     string
	}{
		{name: "text format from GITLAB_LOG_FORMAT", envLogFormat: "text", expected: expectedText},
		{name: "text format from config file", logFormat: "text", expected: expectedText},
		{name: "case insensitive text format from config file", logFormat: "TeXt", expected: expectedText},
		{name: "config json does not override environment format", logFormat: jsonFormat, envLogFormat: "TEXT", expected: expectedText},
		{name: "json format from config file", logFormat: jsonFormat, expected: expectedJSON},
		{name: "unknown format from config file falls back to json", logFormat: "color", expected: expectedJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITLAB_LOG_FORMAT", tc.envLogFormat)
			t.Setenv("GITLAB_LOG_LEVEL", "")
			tmpFile := createTempFile(t)
			closer := ConfigureLogger(&config.Config{LogFile: tmpFile, LogFormat: tc.logFormat})
			if closer != nil {
				defer MustClose(t, closer)
			}
			slog.Info("text format test")

			data, err := os.ReadFile(tmpFile)
			require.NoError(t, err)
			require.Contains(t, string(data), tc.expected)
		})
	}

	t.Run("log level from config file", func(t *testing.T) {
		t.Setenv("GITLAB_LOG_FORMAT", "")
		tmpFile := createTempFile(t)
		cfg := config.Config{
			LogFile:  tmpFile,
			LogLevel: "debug",
		}
		t.Setenv("GITLAB_LOG_LEVEL", "")

		closer := ConfigureLogger(&cfg)
		if closer != nil {
			defer MustClose(t, closer)
		}
		slog.Info("this is a test")
		slog.Debug("debug log message")

		data, err := os.ReadFile(tmpFile)
		dataStr := string(data)
		require.NoError(t, err)
		require.Contains(t, dataStr, `"msg":"this is a test"`)
		require.Contains(t, dataStr, `"msg":"debug log message"`)
	})

	t.Run("log level from GITLAB_LOG_LEVEL env var", func(t *testing.T) {
		t.Setenv("GITLAB_LOG_FORMAT", "")
		tmpFile := createTempFile(t)
		cfg := config.Config{
			LogFile: tmpFile,
		}
		t.Setenv("GITLAB_LOG_LEVEL", "debug")

		closer := ConfigureLogger(&cfg)
		if closer != nil {
			defer MustClose(t, closer)
		}
		slog.Info("this is a test")
		slog.Debug("debug log message")

		data, err := os.ReadFile(tmpFile)
		dataStr := string(data)
		require.NoError(t, err)
		require.Contains(t, dataStr, `"msg":"this is a test"`)
		require.Contains(t, dataStr, `"msg":"debug log message"`)
	})
}

// MustClose calls Close() on the Closer and fails the test in case it returns
// an error. This function is useful when closing via `defer`, as a simple
// `defer require.NoError(t, closer.Close())` would cause `closer.Close()` to
// be executed early already.
func MustClose(tb testing.TB, closer io.Closer) {
	require.NoError(tb, closer.Close())
}

func TestConfigureLoggerDirectoryFailure(t *testing.T) {
	tempDir := t.TempDir()

	config := config.Config{
		LogFile:   tempDir,
		LogFormat: "json",
	}

	fileInfo, err := os.Stat(tempDir)
	require.NoError(t, err)
	assert.True(t, fileInfo.IsDir())

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	closer := ConfigureLogger(&config)
	assert.Nil(t, closer)
	slog.Info("this is a test")

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	os.Stderr = old

	assert.Contains(t, buf.String(), "failed to configure log file", "capture the error in stderr")
	assert.Contains(t, buf.String(), "this is a test", "we should still be logging to stderr in this case")
}

package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bhav-07/pathpilot/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func New(cfg *config.Config) *zap.SugaredLogger {
	logFile, err := setupLogFile(cfg)
	if err != nil {
		panic(fmt.Sprintf("Failed to set up log file: %v", err))
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(logFile),
		getLogLevel(cfg.LogLevel),
	)

	logger := zap.New(core)
	return logger.Sugar()
}

func setupLogFile(cfg *config.Config) (*os.File, error) {
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := filepath.Join(cfg.LogDir, "pathpilot.log")

	if info, err := os.Stat(logPath); err == nil {
		if info.Size() > cfg.MaxLogFileSize {
			if err := os.Rename(logPath, logPath+".old"); err != nil {
				return nil, fmt.Errorf("failed to rotate log file: %w", err)
			}
		}
	}
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func getLogLevel(level string) zapcore.Level {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return zapcore.DebugLevel
	case "INFO":
		return zapcore.InfoLevel
	case "WARN":
		return zapcore.WarnLevel
	case "ERROR":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

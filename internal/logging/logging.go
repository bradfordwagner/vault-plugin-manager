// Package logging provides a process-wide zap logger whose level can be changed
// at runtime, so the manager can honor the logLevel that lives in the watched
// ConfigMap's settings block.
package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	level  = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger *zap.SugaredLogger
)

func init() {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "timestamp"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	c := zap.Config{
		EncoderConfig:    encoderCfg,
		Encoding:         "console",
		ErrorOutputPaths: []string{"stderr"},
		Level:            level,
		OutputPaths:      []string{"stderr"},
	}
	l, _ := c.Build()
	logger = l.Sugar()
}

// Log returns the shared sugared logger.
func Log() *zap.SugaredLogger { return logger }

// SetLevel sets the global log level from a string: debug | info | warn | error.
// It is safe to call concurrently and takes effect immediately.
func SetLevel(s string) error {
	switch s {
	case "debug":
		level.SetLevel(zapcore.DebugLevel)
	case "info":
		level.SetLevel(zapcore.InfoLevel)
	case "warn":
		level.SetLevel(zapcore.WarnLevel)
	case "error":
		level.SetLevel(zapcore.ErrorLevel)
	default:
		return fmt.Errorf("invalid log level %q", s)
	}
	return nil
}

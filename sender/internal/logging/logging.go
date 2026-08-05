// Package logging builds the sender's logger and lets its level change while running.
package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// Logger is a zap logger whose level can be raised and lowered at runtime.
//
// The atomic level is the reason this type exists rather than a bare *zap.Logger. Turning up
// verbosity is what an operator does when something is going wrong, and that is exactly the
// moment when restarting the process to change a setting is the least acceptable — a restart
// loses the state that was about to explain the problem.
type Logger struct {
	*zap.Logger
	level zap.AtomicLevel
}

// New builds a logger from configuration.
func New(cfg config.Log) (*Logger, error) {
	level, err := zapcore.ParseLevel(cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("logging: %w", err)
	}
	atomic := zap.NewAtomicLevelAt(level)

	var zcfg zap.Config
	switch cfg.Format {
	case "console":
		zcfg = zap.NewDevelopmentConfig()
	default:
		zcfg = zap.NewProductionConfig()
		// ISO timestamps rather than epoch floats: these lines are read by people correlating
		// them against a receiver's log across an air gap, and epoch seconds make that needlessly
		// hard.
		zcfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}
	zcfg.Level = atomic

	logger, err := zcfg.Build()
	if err != nil {
		return nil, fmt.Errorf("logging: %w", err)
	}
	return &Logger{Logger: logger, level: atomic}, nil
}

// SetLevel changes the level. An unparseable level is reported rather than applied, so a
// mistyped reload does not silently leave logging where it was.
func (l *Logger) SetLevel(name string) error {
	level, err := zapcore.ParseLevel(name)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	l.level.SetLevel(level)
	return nil
}

// Level is the level now in force.
func (l *Logger) Level() string { return l.level.Level().String() }

// Sync flushes buffered lines.
func (l *Logger) Sync() error {
	// Stderr on some platforms reports an error on sync that means nothing; it is ignored here
	// because a shutdown path that failed on it would be reporting a problem that is not one.
	_ = l.Logger.Sync()
	return nil
}

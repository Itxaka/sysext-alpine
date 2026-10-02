package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// syslog levels, as used by systemd's log_*() functions.
const (
	logEmerg = iota
	logAlert
	logCrit
	logErr
	logWarning
	logNotice
	logInfo
	logDebug
)

var logLevelNames = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// logTargets are the targets a per-target level may be given for; only the
// console levels matter, everything is logged to stderr.
var logTargets = []string{"console", "console-prefixed", "kmsg", "journal", "syslog"}

// logger writes systemd-style log lines, plain messages without prefix, to
// stderr, dropping those above the maximum level (info by default).
type logger struct {
	w     io.Writer
	level int
}

// newLogger returns a logger whose level honours $SYSTEMD_LOG_LEVEL and
// $DEBUG_INVOCATION like log_parse_environment().
func newLogger(w io.Writer) *logger {
	l := &logger{w: w, level: logInfo}
	if v, ok := os.LookupEnv("SYSTEMD_LOG_LEVEL"); ok {
		level, err := parseLogLevels(v, l.level)
		if err != nil {
			l.Warnf("Failed to parse log level '%s', ignoring: Invalid argument", v)
		} else {
			l.level = level
		}
	} else if v, ok := os.LookupEnv("DEBUG_INVOCATION"); ok {
		b, err := release.ParseBoolean(v)
		switch {
		case err != nil:
			l.Warnf("Failed to parse $DEBUG_INVOCATION value, ignoring: Invalid argument")
		case b:
			l.level = logDebug
		}
	}
	return l
}

// parseLogLevels is log_set_max_level_from_string(): a comma-separated list
// of levels and TARGET:LEVEL pairs. A message reaches the console when it is
// within both the global and the console target's maximum level.
func parseLogLevels(s string, level int) (int, error) {
	console := logDebug
	for word := range strings.SplitSeq(s, ",") {
		if word == "" {
			continue
		}
		target, value, ok := strings.Cut(word, ":")
		if !ok {
			value = word
		} else if !slices.Contains(logTargets, target) {
			return 0, fmt.Errorf("unknown log target '%s'", target)
		}
		l, err := parseLogLevel(value)
		if err != nil {
			return 0, err
		}
		switch {
		case !ok:
			level = l
		case target == "console":
			console = l
		}
	}
	return min(level, console), nil
}

// parseLogLevel is log_level_from_string(): a level name or number 0-7.
func parseLogLevel(s string) (int, error) {
	for i, n := range logLevelNames {
		if s == n {
			return i, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < logEmerg || n > logDebug {
		return 0, fmt.Errorf("invalid log level '%s'", s)
	}
	return n, nil
}

func (l *logger) logf(level int, format string, args ...any) {
	if level > l.level {
		return
	}
	fmt.Fprintf(l.w, format+"\n", args...)
}

func (l *logger) Debugf(format string, args ...any)  { l.logf(logDebug, format, args...) }
func (l *logger) Infof(format string, args ...any)   { l.logf(logInfo, format, args...) }
func (l *logger) Noticef(format string, args ...any) { l.logf(logNotice, format, args...) }
func (l *logger) Warnf(format string, args ...any)   { l.logf(logWarning, format, args...) }
func (l *logger) Errorf(format string, args ...any)  { l.logf(logErr, format, args...) }

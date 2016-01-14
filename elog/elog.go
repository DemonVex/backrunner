package elog

import (
	"io"
	"log"
	"os"
)

const (
	LevelFatal   = 1
	LevelError   = 2
	LevelWarning = 3
	LevelInfo    = 4
	LevelDebug   = 5
)

var std = log.New(os.Stderr, "", log.LstdFlags)
var logLevel = 4
var logLevelName = make(map[string]int)
var logFile *os.File

func init() {
	logLevelName["fatal"] = 1
	logLevelName["error"] = 2
	logLevelName["warning"] = 3
	logLevelName["info"] = 4
	logLevelName["debug"] = 5
}

func SetOutputFile(fileName string) {
	var err error
	if logFile != nil {
		logFile.Close()
	}

	logFile, err = os.OpenFile(fileName, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		std.Fatalf("Could not open log file '%s': %q", fileName, err)
	}
}

func SetOutput(w io.Writer) {
	std.SetOutput(w)
}

func Flags() int {
	return std.Flags()
}

func SetFlags(flag int) {
	std.SetFlags(flag)
}

func Prefix() string {
	return std.Prefix()
}

func SetPrefix(prefix string) {
	std.SetPrefix(prefix)
}

func LogLevel() int {
	return logLevel
}

func SetLogLevel(level string) {
	logLevel = logLevelName[level]
}

func checkLevel(level int) bool {
	if level <= logLevel {
		return true
	}
	return false
}

func Debug(v ...interface{}) {
	if checkLevel(LevelDebug) {
		std.Print(v...)
	}
}

func Debugf(format string, v ...interface{}) {
	if checkLevel(LevelDebug) {
		std.Printf(format, v...)
	}
}
func Info(v ...interface{}) {
	if checkLevel(LevelInfo) {
		std.Print(v...)
	}
}

func Infof(format string, v ...interface{}) {
	if checkLevel(LevelInfo) {
		std.Printf(format, v...)
	}
}

func Warning(v ...interface{}) {
	if checkLevel(LevelWarning) {
		std.Print(v...)
	}
}

func Warningf(format string, v ...interface{}) {
	if checkLevel(LevelWarning) {
		std.Printf(format, v...)
	}
}
func Error(v ...interface{}) {
	if checkLevel(LevelError) {
		std.Print(v...)
	}
}

func Errorf(format string, v ...interface{}) {
	if checkLevel(LevelError) {
		std.Printf(format, v...)
	}
}

func Fatal(v ...interface{}) {
	if checkLevel(LevelFatal) {
		std.Fatal(v...)
	}
}

func Fatalf(format string, v ...interface{}) {
	if checkLevel(LevelFatal) {
		std.Fatalf(format, v...)
	}
}

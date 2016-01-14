package alog

import (
	"io"
	"log"
	"os"

	"github.com/DemonVex/backrunner/elog"
)

var std = log.New(os.Stderr, "", log.LstdFlags)
var logFile *os.File

func SetOutputFile(fileName string) {
	var err error
	if logFile != nil {
		logFile.Close()
	}

	logFile, err = os.OpenFile(fileName, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		elog.Fatalf("Could not open log file '%s': %q", fileName, err)
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

func Print(v ...interface{}) {
	std.Print(v...)
}

func Printf(format string, v ...interface{}) {
	std.Printf(format, v...)
}

func Println(v ...interface{}) {
	std.Println(v...)
}

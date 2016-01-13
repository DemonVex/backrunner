package alog

import (
	"io"
	"log"
	"os"
)

var std = log.New(os.Stderr, "", log.LstdFlags)

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

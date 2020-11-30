package logutil

import (
	"fmt"
	"os"

	"github.com/mgutz/ansi"
)

var (
	EnableDebug = false

	Yel   = ansi.ColorFunc("yellow")
	Green = ansi.ColorFunc("green")
	Red   = ansi.ColorFunc("red")
	Bold  = ansi.ColorFunc("white+b")
	Gray  = ansi.ColorFunc("black+h")
)

// Prints to stderr.
func Debugf(format string, a ...interface{}) {
	if !EnableDebug {
		return
	}
	fmt.Fprintf(os.Stderr, "%s: ", Gray("debug"))
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Prints to stderr.
func Errorf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s: ", Red("error"))
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Prints to stderr.
func Infof(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s: ", Yel("info"))
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

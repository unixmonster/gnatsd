package logger

import (
	"bytes"
	"io"
	"io/ioutil"
	"log"
	"os"
	"testing"
)

func TestStdLogger(t *testing.T) {
	logger := NewStdLogger(false, false, false, false)

	flags := logger.logger.Flags()
	if flags != 0 {
		t.Fatalf("Expected %q, received %q\n", 0, flags)
	}

	if logger.debug {
		t.Fatalf("Expected %t, received %t\n", false, logger.debug)
	}

	if logger.trace {
		t.Fatalf("Expected %t, received %t\n", false, logger.trace)
	}
}

func TestStdLoggerWithDebugTraceAndTime(t *testing.T) {
	logger := NewStdLogger(true, true, true, false)

	flags := logger.logger.Flags()
	if flags != log.LstdFlags {
		t.Fatalf("Expected %d, received %d\n", log.LstdFlags, flags)
	}

	if !logger.debug {
		t.Fatalf("Expected %t, received %t\n", true, logger.debug)
	}

	if !logger.trace {
		t.Fatalf("Expected %t, received %t\n", true, logger.trace)
	}
}

func TestStdLoggerNotice(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, false, false, false)
		logger.Notice("foo")
	}, "[INFO] foo\n")
}

func TestStdLoggerNoticeWithColor(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, false, false, true)
		logger.Notice("foo")
	}, "[\x1b[32mINFO\x1b[0m] foo\n")
}

func TestStdLoggerDebug(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, true, false, false)
		logger.Debug("foo %s", "bar")
	}, "[DEBUG] foo bar\n")
}

func TestStdLoggerDebugWithOutDebug(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, false, false, false)
		logger.Debug("foo")
	}, "")
}

func TestStdLoggerTrace(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, false, true, false)
		logger.Trace("foo")
	}, "[TRACE] foo\n")
}

func TestStdLoggerTraceWithOutDebug(t *testing.T) {
	expectOutput(t, func() {
		logger := NewStdLogger(false, false, false, false)
		logger.Trace("foo")
	}, "")
}

func TestFileLogger(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "_gnatsd")
	if err != nil {
		t.Fatal("Could not create tmp dir")
	}
	defer os.RemoveAll(tmpDir)

	file, err := ioutil.TempFile(tmpDir, "gnatsd:log_")
	file.Close()

	logger := NewFileLogger(file.Name(), false, false, false)
	logger.Notice("foo")

	buf, err := ioutil.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("Could not read logfile: %v", err)
	}
	if len(buf) <= 0 {
		t.Fatal("Expected a non-zero length logfile")
	}

	if string(buf) != "[INFO] foo\n" {
		t.Fatalf("Expected '%s', received '%s'\n", "[INFO] foo", string(buf))
	}
}

func expectOutput(t *testing.T, f func(), expected string) {
	old := os.Stderr // keep backup of the real stdout
	r, w, _ := os.Pipe()
	os.Stderr = w

	f()

	outC := make(chan string)
	// copy the output in a separate goroutine so printing can't block indefinitely
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	os.Stderr.Close()
	os.Stderr = old // restoring the real stdout
	out := <-outC
	if out != expected {
		t.Fatalf("Expected '%s', received '%s'\n", expected, out)
	}
}
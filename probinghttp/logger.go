/*
original:
https://github.com/prometheus-community/pro-bing/blob/v0.4.1/logger.go

The MIT License (MIT)

Copyright 2022 The Prometheus Authors
Copyright 2016 Cameron Sparr and contributors.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package probinghttp

import "log"

type Logger interface {
	Fatalf(format string, v ...interface{})
	Errorf(format string, v ...interface{})
	Warnf(format string, v ...interface{})
	Infof(format string, v ...interface{})
	Debugf(format string, v ...interface{})
}

type StdLogger struct {
	Logger *log.Logger
}

func (l StdLogger) Fatalf(format string, v ...interface{}) {
	l.Logger.Printf("FATAL: "+format, v...)
}

func (l StdLogger) Errorf(format string, v ...interface{}) {
	l.Logger.Printf("ERROR: "+format, v...)
}

func (l StdLogger) Warnf(format string, v ...interface{}) {
	l.Logger.Printf("WARN: "+format, v...)
}

func (l StdLogger) Infof(format string, v ...interface{}) {
	l.Logger.Printf("INFO: "+format, v...)
}

func (l StdLogger) Debugf(format string, v ...interface{}) {
	l.Logger.Printf("DEBUG: "+format, v...)
}

type NoopLogger struct {
}

func (l NoopLogger) Fatalf(format string, v ...interface{}) {
}

func (l NoopLogger) Errorf(format string, v ...interface{}) {
}

func (l NoopLogger) Warnf(format string, v ...interface{}) {
}

func (l NoopLogger) Infof(format string, v ...interface{}) {
}

func (l NoopLogger) Debugf(format string, v ...interface{}) {
}

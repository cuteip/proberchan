package probers

import (
	probehttp "github.com/cuteip/proberchan/probers/http"
	probeping "github.com/cuteip/proberchan/probers/ping"
)

type RunningProbers struct {
	Ping map[string]*probeping.Runner
	HTTP map[string]*probehttp.Runner
}

func NewRunningProbers() *RunningProbers {
	return &RunningProbers{
		Ping: make(map[string]*probeping.Runner),
		HTTP: make(map[string]*probehttp.Runner),
	}
}

func (r *RunningProbers) AddPing(name string, runner *probeping.Runner) {
	r.Ping[name] = runner
}

func (r *RunningProbers) AddHTTP(name string, runner *probehttp.Runner) {
	r.HTTP[name] = runner
}

func (r *RunningProbers) GetPing(name string) (*probeping.Runner, bool) {
	prober, exist := r.Ping[name]
	return prober, exist
}

func (r *RunningProbers) GetHTTP(name string) (*probehttp.Runner, bool) {
	prober, exist := r.HTTP[name]
	return prober, exist
}

func (r *RunningProbers) RemovePing(name string) {
	prober, exist := r.GetPing(name)
	if exist {
		prober.Stop()
	}
	delete(r.Ping, name)
}

func (r *RunningProbers) RemoveHTTP(name string) {
	prober, exist := r.GetHTTP(name)
	if exist {
		prober.Stop()
	}
	delete(r.HTTP, name)
}

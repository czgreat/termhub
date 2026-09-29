//go:build windows

// Command termhub-agent is the per-machine program of termhub. One executable
// plays several roles, selected by a subcommand (docs/M5 第 2 节). Only the
// roles implemented so far are listed here.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"termhub/internal/agent"
	"termhub/internal/host"
	"termhub/internal/proto"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runAgent()
	case "host":
		runHost()
	case "enroll":
		enroll()
	case "version":
		fmt.Printf("termhub-agent %s (protocol %s)\n", version, proto.Current)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  termhub-agent enroll --hub https://<hub>:27443 --token thn_... [--pin <sha256 of the Hub certificate>] [--name <label>]
  termhub-agent run      connect this machine to the Hub (what the scheduled task starts)
  termhub-agent host     the session host; started by "run" when needed
  termhub-agent version`)
	os.Exit(2)
}

func logger(sub string) *slog.Logger {
	dir := filepath.Join(agent.Dir(), sub, "logs")
	os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, sub+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		f = os.Stderr
	}
	return slog.New(slog.NewTextHandler(f, nil))
}

// enroll verifies the Hub address, certificate and token with a real
// connection first, and writes the configuration only when that worked.
func enroll() {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	hub := fs.String("hub", "", "Hub address, https://host:port")
	token := fs.String("token", "", "node token shown once by the Hub")
	pin := fs.String("pin", "", "SHA-256 fingerprint of the Hub's certificate (needed for a self-signed Hub)")
	name := fs.String("name", "", "label for this machine (default: the computer name)")
	fs.Parse(os.Args[2:])
	if *hub == "" || *token == "" {
		usage()
	}
	tlsc, err := agent.TLSConfig(*pin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enrol failed:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.Probe(ctx, agent.Config{HubURL: *hub, Token: *token, TLS: tlsc}); err != nil {
		fmt.Fprintln(os.Stderr, "enrol failed:", err)
		fmt.Fprintln(os.Stderr, "nothing was written.")
		os.Exit(1)
	}
	if err := agent.Save(agent.Dir(), agent.File{HubURL: *hub, NodeName: *name, CertPin: *pin}, *token); err != nil {
		fmt.Fprintln(os.Stderr, "enrol failed:", err)
		os.Exit(1)
	}
	fmt.Println("enrolled. The Hub accepted this machine; the token is stored sealed for this Windows user in", agent.Dir())
	fmt.Println("start it with: termhub-agent run")
}

func runAgent() {
	// -pipe exists for tests and development, so that they never talk to a
	// production host running under the same Windows user.
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	pipeFlag := fs.String("pipe", "", "session host pipe (default: per-user name)")
	fs.Parse(os.Args[2:])
	log := logger("agent")
	file, token, err := agent.Load(agent.Dir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		log.Error("cannot start", "err", err.Error())
		os.Exit(1)
	}
	tlsc, err := agent.TLSConfig(file.CertPin)
	if err != nil {
		log.Error("cannot start", "err", err.Error())
		os.Exit(1)
	}
	pipe := *pipeFlag
	if pipe == "" {
		if pipe, err = host.DefaultPipeName(); err != nil {
			log.Error("cannot start", "err", err.Error())
			os.Exit(1)
		}
	}
	agent.Version = version
	a := agent.New(agent.Config{HubURL: file.HubURL, Token: token, NodeName: file.NodeName, PipeName: pipe, TLS: tlsc, Log: log,
		StartHost: func() error { return agent.StartHostProcess(*pipeFlag) }})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	log.Info("agent started", "version", version, "hub", file.HubURL)
	a.Run(ctx) // ending the agent never ends the host or its sessions
	log.Info("agent stopped")
}

// runHost is the session host (docs/M3). It has no console, no network and no
// credentials; it serves the agent on a per-user named pipe.
func runHost() {
	// -pipe and -state exist for tests and for running two hosts side by side
	// during development; production uses the per-user defaults.
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	pipe := fs.String("pipe", "", "named pipe to serve (default: per-user name)")
	state := fs.String("state", filepath.Join(agent.Dir(), "host"), "state and log directory")
	fs.Parse(os.Args[2:])
	dir := *state
	os.MkdirAll(filepath.Join(dir, "logs"), 0o700)
	logFile, err := os.OpenFile(filepath.Join(dir, "logs", "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		logFile = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logFile, nil))
	host.Version = version
	h, err := host.New(host.Config{PipeName: *pipe, StateDir: dir, Log: log})
	if err != nil {
		// Most likely another host already serves this user: that is fine.
		log.Error("host not started", "err", err)
		os.Exit(1)
	}
	log.Info("host started", "version", version)
	h.Wait()
	log.Info("host stopped")
}

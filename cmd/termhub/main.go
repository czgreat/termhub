// Command termhub is the Hub (docs/M8). Configuration comes from TH_*
// environment variables; state lives in TH_DATA_DIR.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // the image has no /usr/share/zoneinfo (deploy/Dockerfile.prebuilt)

	"termhub/internal/hub/auth"
	"termhub/internal/hub/server"
	"termhub/internal/hub/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("termhub", version)
			return
		case "admin":
			admin(os.Args[2:])
			return
		case "healthcheck":
			healthcheck()
			return
		default:
			fmt.Fprintln(os.Stderr, "usage: termhub [version | healthcheck | admin reset-totp <username>]")
			os.Exit(2)
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := server.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Error("configuration", "err", err.Error())
		os.Exit(2)
	}
	cfg.Log = log
	s, err := server.New(cfg, os.Getenv)
	if err != nil {
		log.Error("cannot start", "err", err.Error())
		os.Exit(1)
	}
	log.Info("termhub started", "version", version, "lan", s.LANAddr(), "proxy", s.ProxyAddr(), "cert_fingerprint", s.Fingerprint)
	if t := s.Auth.SetupToken(); t != "" {
		// The only place this token ever appears: whoever can read the
		// container's log owns the machine anyway.
		log.Warn("no users yet: open the Hub in a browser and create the first administrator with this one-time setup token", "setup_token", t)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	s.Close()
	log.Info("termhub stopped")
}

// healthcheck is what the container's health probe runs: the image has no
// shell and no curl. It asks the running Hub's own /healthz over the LAN
// listener and accepts exactly the certificate stored in the data directory.
func healthcheck() {
	dir := os.Getenv("TH_DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	listen := os.Getenv("TH_LAN_LISTEN")
	if listen == "" {
		listen = ":27443"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "TH_LAN_LISTEN:", err)
		os.Exit(1)
	}
	pemBytes, err := os.ReadFile(filepath.Join(dir, "tls", "hub.crt"))
	block, _ := pem.Decode(pemBytes)
	if err != nil || block == nil {
		fmt.Fprintln(os.Stderr, "cannot read the Hub certificate:", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // replaced by the exact match below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, block.Bytes) {
				return fmt.Errorf("the listener does not present the Hub's certificate")
			}
			return nil
		}}}}
	resp, err := client.Get("https://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		os.Exit(1)
	}
	fmt.Println("ok")
}

// admin is the break-glass path, run with `docker exec` by whoever owns the
// NAS (docs/M6 第 10 节).
func admin(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: termhub admin reset-totp <username>")
		fmt.Fprintln(os.Stderr, "       termhub admin create-user <username> [admin|user]")
		os.Exit(2)
	}
	if len(args) < 2 || (args[0] != "reset-totp" && args[0] != "create-user") {
		usage()
	}
	role := "user"
	if args[0] == "create-user" && len(args) == 3 {
		role = args[2]
	} else if len(args) != 2 {
		usage()
	}
	dir := os.Getenv("TH_DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	key, err := os.ReadFile(filepath.Join(dir, "master.key"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read the master key:", err)
		os.Exit(1)
	}
	db, err := store.Open(filepath.Join(dir, "termhub.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	hp, err := auth.HashParamsFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	svc, err := auth.New(db, key, auth.Config{Hash: hp})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if args[0] == "create-user" {
		temp, err := svc.CreateUserLocal(args[1], role)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("user %q (%s) created. Temporary password, shown only now: %s\nThey must change it and enrol a second factor at first login.\n", args[1], role, temp)
		return
	}
	temp, err := svc.ResetTOTPLocal(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("second factor of %q removed; all their sessions and trusted devices are revoked.\nTemporary password, shown only now: %s\nAt next login they must change it and enrol a new authenticator.\n", args[1], temp)
}

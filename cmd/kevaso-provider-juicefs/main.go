// Command kevaso-provider-juicefs is Kevaso's JuiceFS provider plugin, one program with two sides:
//
//	kevaso-provider-juicefs serve --config FILE [--socket /run/kevaso-plugins/juicefs/plugin.sock]
//	kevaso-provider-juicefs node  --config FILE [--socket /run/kevaso-plugins/juicefs/node.sock]
//
// serve is the controller side, on control planes: it formats each class's JuiceFS file system
// once and creates and deletes the volumes' folders. node is the node side, on workers: it mounts
// a volume's folder where kevasod asks. Both run the juicefs command (on PATH, or --juicefs) and
// read the classes' credentials from systemd credentials (LoadCredential=NAME:FILE), or, outside
// systemd, from the folder KEVASO_JUICEFS_CREDENTIALS_DIR names; never from argv or the config.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"kevaso.com/core/sdk/kplugin"
	"kevaso.com/provider-juicefs/juicefs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kevaso-provider-juicefs serve|node --config FILE [--socket PATH] [--juicefs BINARY] | version")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		log.Printf("kevaso-provider-juicefs: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	if cmd == "version" {
		fmt.Println(juicefs.Version)
		return nil
	}
	if cmd != "serve" && cmd != "node" {
		return fmt.Errorf("unknown command %q", cmd)
	}
	sock := kplugin.ControllerSocket
	if cmd == "node" {
		sock = kplugin.NodeSocket
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	socket := fs.String("socket", filepath.Join(kplugin.Dir, juicefs.Name, sock), "where kevasod finds this side")
	cfgFile := fs.String("config", "", "settings (JSON): classes, their bucket and metadata URL, and the names of their credentials")
	bin := fs.String("juicefs", "juicefs", "the juicefs command")
	_ = fs.Parse(args)
	if *cfgFile == "" {
		return errors.New("--config is needed: it lists the classes")
	}
	b, err := os.ReadFile(*cfgFile)
	if err != nil {
		return err
	}
	var cfg juicefs.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s: %w", *cfgFile, err)
	}
	if err := cfg.Check(); err != nil {
		return fmt.Errorf("%s: %w", *cfgFile, err)
	}
	runner := juicefs.Exec{Binary: *bin}
	if cmd == "serve" {
		p := juicefs.New(cfg, runner, credential)
		p.Log = log.Printf
		log.Printf("juicefs provider %s: controller side serving %d class(es) on %s", juicefs.Version, len(cfg.Classes), *socket)
		return kplugin.Serve(ctx, *socket, p)
	}
	n := juicefs.NewNode(cfg, runner, credential)
	n.Log = log.Printf
	log.Printf("juicefs provider %s: node side serving %d class(es) on %s", juicefs.Version, len(cfg.Classes), *socket)
	return kplugin.Serve(ctx, *socket, n)
}

// credential reads a systemd credential, or outside systemd a file in KEVASO_JUICEFS_CREDENTIALS_DIR.
func credential(name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("credential name %q", name)
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		dir = os.Getenv("KEVASO_JUICEFS_CREDENTIALS_DIR")
	}
	if dir == "" {
		return "", errors.New("no credentials: run under systemd with LoadCredential=NAME:FILE (kvoadm plugin install --credential NAME=FILE)")
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("credential %s is empty", name)
	}
	return v, nil
}

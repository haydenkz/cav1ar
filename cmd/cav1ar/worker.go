package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/haydenkz/cav1ar/internal/config"
	"github.com/haydenkz/cav1ar/internal/encode"
	"github.com/haydenkz/cav1ar/internal/worker"
)

// listenForWorkers starts taking workers for the daemon's queue, making the
// token they need if there isn't one yet.
func listenForWorkers(s encode.Settings, q *encode.Queue) (*worker.Server, error) {
	token, path, err := config.WorkerToken(true)
	if err != nil {
		return nil, err
	}
	ws, err := worker.Listen(s.WorkerListen, token, q)
	if err != nil {
		return nil, err
	}
	log.Printf("cav1ar: taking workers on %s, sharing %s; they connect with the token in %s", s.WorkerListen, s.WorkerDir, path)
	return ws, nil
}

// runWorker lends this machine's devices to the daemon at addr until it is
// stopped.
func runWorker(addr, name, allow string) error {
	if toJournal() {
		log.SetFlags(0) // the journal adds its own timestamps
	}
	token := os.Getenv("CAV1AR_WORKER_TOKEN")
	if token == "" {
		var err error
		if token, _, err = config.WorkerToken(false); err != nil {
			return err
		}
	}
	var dirs []string
	for dir := range strings.SplitSeq(allow, ",") {
		if dir = strings.TrimSpace(dir); dir != "" {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			dirs = append(dirs, abs)
		}
	}
	devices := encode.Devices()
	for _, d := range devices {
		log.Printf("cav1ar: lending %s (%s)", d.Name, d.ID)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return worker.Work(ctx, worker.Options{Addr: addr, Token: token, Name: name, Allow: dirs, Devices: devices, Log: log.Printf})
}

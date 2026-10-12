package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/EFR-GmbH/eebus-arena/internal/arena"
	"github.com/EFR-GmbH/eebus-arena/internal/webui"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func env(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
func main() {
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		client()
		return
	}
	data, err := filepath.Abs(env("ARENA_DATA", ".arena"))
	if err != nil {
		log.Fatal(err)
	}
	docker, err := arena.NewDocker()
	if err != nil {
		log.Fatal(err)
	}
	controller, err := arena.NewController(data, env("ARENA_HOST_DATA", data), docker)
	if err != nil {
		log.Fatal(err)
	}
	defer controller.Close()
	server := &http.Server{Addr: env("ARENA_LISTEN", "127.0.0.1:8080"), Handler: controller.Handler(webui.Files()), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serverError := make(chan error, 1)
	go func() {
		log.Printf("arena listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			serverError <- err
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
	case err := <-serverError:
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
func client() {
	flags := flag.NewFlagSet("arena", flag.ExitOnError)
	server := flags.String("server", "http://localhost:8080", "arena server URL")
	flags.Parse(os.Args[1:])
	args := flags.Args()
	if len(args) == 0 {
		log.Fatal("commands: state, create FILE, start RUN, stop RUN, test RUN, export RUN, action RUN NODE ACTION, command RUN NODE FILE")
	}
	method, path, body := "GET", "/api/v1/state", io.Reader(nil)
	switch args[0] {
	case "pairing-reset":
		if len(args) != 3 {
			log.Fatal("pairing-reset RUN NODE")
		}
		method, path, body = "POST", "/api/v1/runs/"+args[1]+"/nodes/"+args[2]+"/pairing/reset", strings.NewReader("{}")
	case "state":
	case "create":
		if len(args) != 2 {
			log.Fatal("create FILE")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			log.Fatal(err)
		}
		body = strings.NewReader(string(b))
		method = "POST"
		path = "/api/v1/runs"
	case "start", "stop", "test", "export":
		if len(args) != 2 {
			log.Fatal("expected run ID")
		}
		path = "/api/v1/runs/" + args[1] + "/" + args[0]
		if args[0] != "export" {
			method = "POST"
			body = strings.NewReader("{}")
		}
	case "action":
		if len(args) != 4 {
			log.Fatal("action RUN NODE ACTION")
		}
		method = "POST"
		path = "/api/v1/runs/" + args[1] + "/nodes/" + args[2] + "/actions"
		body = strings.NewReader(fmt.Sprintf(`{"action":%q}`, args[3]))
	case "command":
		if len(args) != 4 {
			log.Fatal("command RUN NODE FILE")
		}
		b, err := os.ReadFile(args[3])
		if err != nil {
			log.Fatal(err)
		}
		method = "POST"
		path = "/api/v1/runs/" + args[1] + "/nodes/" + args[2] + "/commands"
		body = strings.NewReader(string(b))
	default:
		log.Fatal("unknown command")
	}
	req, err := http.NewRequest(method, *server+path, body)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(os.Stdout, response.Body)
	if response.StatusCode >= 300 {
		os.Exit(1)
	}
}

// arena-agent runs one managed native EEBUS stub on a dedicated physical lab node.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/arena"
	"github.com/EFR-GmbH/eebus-arena/internal/labagent"
)

func main() {
	root := flag.String("root", "/tmp/arena-stub", "Managed temporary data directory")
	listen := flag.String("listen", "127.0.0.1:8843", "Lab address to listen on")
	native := flag.String("native", "/tmp/arena-stub/arena-node", "Native stub executable")
	supervisor := flag.String("supervisor", "/tmp/arena-stub/arena-supervisor", "Resource supervisor executable")
	init := flag.Bool("init", false, "Create transport identity and token once")
	flag.Parse()
	certPath, keyPath, tokenPath := filepath.Join(*root, "agent-cert.pem"), filepath.Join(*root, "agent-key.pem"), filepath.Join(*root, "token")
	if *init {
		if err := os.MkdirAll(*root, 0700); err != nil {
			log.Fatal(err)
		}
		for _, path := range []string{certPath, keyPath, tokenPath} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				log.Fatal("transport identity already exists or cannot be checked")
			}
		}
		identity, err := arena.NewIdentity("arena-physical-agent")
		if err != nil {
			log.Fatal(err)
		}
		for path, body := range map[string]string{certPath: identity.Certificate, keyPath: identity.PrivateKey} {
			if err = os.WriteFile(path, []byte(body), 0600); err != nil {
				log.Fatal(err)
			}
		}
		var token [32]byte
		if _, err = rand.Read(token[:]); err != nil {
			log.Fatal(err)
		}
		if err = os.WriteFile(tokenPath, []byte(hex.EncodeToString(token[:])), 0600); err != nil {
			log.Fatal(err)
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			log.Fatal(err)
		}
		hash := sha256.Sum256(cert.Certificate[0])
		fmt.Printf("TLS SHA-256: %x\nToken file: %s\n", hash, tokenPath)
		return
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		log.Fatal(err)
	}
	agent := &labagent.Agent{Root: *root, Native: *native, Supervisor: *supervisor, Token: string(token)}
	server := &http.Server{Addr: *listen, Handler: agent.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := agent.Shutdown(cleanup); err != nil {
			log.Print(err)
		}
		if err := server.Shutdown(cleanup); err != nil {
			log.Print(err)
		}
	}()
	if err = server.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

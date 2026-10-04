// replay-relay runs independently of CS2, OBS and the LAN replay service.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"projectreplay/internal/relay"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", ":7790", "HTTP bind address for direct IP access (use 127.0.0.1:7790 behind a proxy)")
	data := flag.String("data", "relay-data", "private relay state/cache directory")
	quota := flag.Int64("cache-gib", 10, "temporary cache quota in GiB")
	cert := flag.String("tls-cert", "", "TLS certificate file (optional behind proxy)")
	key := flag.String("tls-key", "", "TLS private key file")
	flag.Parse()
	if *quota < 1 || *quota > 10240 {
		log.Fatal("cache-gib must be 1..10240")
	}
	broker, e := relay.NewServer(*data, *quota<<30)
	if e != nil {
		log.Fatal(e)
	}
	defer broker.Close()
	server := &http.Server{Addr: *listen, Handler: broker.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	errs := make(chan error, 1)
	go func() {
		if *cert != "" || *key != "" {
			errs <- server.ListenAndServeTLS(*cert, *key)
		} else {
			errs <- server.ListenAndServe()
		}
	}()
	log.Printf("Replay relay listening on %s; direct IP access supported; HTTP is unencrypted", *listen)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case e := <-errs:
		if e != http.ErrServerClosed {
			log.Print(e)
		}
	}
	broker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}

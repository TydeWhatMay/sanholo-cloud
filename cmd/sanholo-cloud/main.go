// Command sanholo-cloud runs the SanHolo Hosted control plane.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lhaig/sanholo-cloud/internal/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	flag.Parse()

	app := server.New()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		_ = app.Shutdown()
	}()
	if err := app.Listen(*addr); err != nil {
		log.Fatal(err)
	}
}

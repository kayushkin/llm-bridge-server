package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	filestore "github.com/kayushkin/file-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	service, err := filestore.Start(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	defer service.Store.Close()

	address := service.Settings.String(filestore.SettingListenAddress)
	httpServer := &http.Server{
		Addr:              address,
		Handler:           service.Server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("file-store listening on %s (data=%s); every route but /health needs %s", address, service.Store.DataDir(), filestore.ServiceTokenHeader)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdown); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

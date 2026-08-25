// Command video-pipe is the scheduling/management layer for the video-pipe
// gateway. It supervises per-stream ffmpeg processes that remux arbitrary input
// sources into RTSP pushed to MediaMTX, and exposes a Web UI + JSON API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"video-pipe/internal/config"
	"video-pipe/internal/manager"
	"video-pipe/internal/mediamtx"
	"video-pipe/internal/ondemand"
	"video-pipe/internal/server"
	"video-pipe/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) > 1 && os.Args[1] == "on-demand-hook" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := ondemand.Run(ctx, log); err != nil {
			log.Error("on-demand hook failed", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// Cancel on SIGINT/SIGTERM for graceful shutdown of the HTTP server and all
	// ffmpeg subprocesses.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Ensure the upload directory exists (under the mounted /data volume).
	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		log.Error("upload dir create failed", "dir", cfg.UploadDir, "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.ProviderCacheDir, 0o755); err != nil {
		log.Error("provider cache dir create failed", "dir", cfg.ProviderCacheDir, "err", err)
		os.Exit(1)
	}

	mtxClient := mediamtx.New(cfg.MediaMTXAPI, cfg.MediaMTXUser, cfg.MediaMTXPass)
	mgr := manager.New(st, mtxClient, cfg.MediaMTXHost, cfg.ProviderCacheDir, log)
	if err := mgr.Start(ctx); err != nil {
		log.Error("manager start failed", "err", err)
		os.Exit(1)
	}

	srv, err := server.New(cfg, st, mgr, log)
	if err != nil {
		log.Error("server init failed", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}
	onDemandSrv := &http.Server{
		Addr:              cfg.OnDemandAddr,
		Handler:           server.NewOnDemandHandler(mgr, log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("video-pipe listening", "addr", cfg.Addr, "playback_host", cfg.PlaybackHost)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		log.Info("on-demand callback listening", "addr", cfg.OnDemandAddr)
		if err := onDemandSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("on-demand callback server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down, stopping ffmpeg processes")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown error", "err", err)
	}
	if err := onDemandSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("on-demand callback shutdown error", "err", err)
	}
	mgr.Wait() // block until every ffmpeg process group has exited
	log.Info("shutdown complete")
}

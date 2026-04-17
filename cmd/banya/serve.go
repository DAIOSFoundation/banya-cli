package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/server"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Expose the local banya-core sidecar over HTTP+SSE for remote clients",
		Long: `Spawns the banya-core sidecar and runs an HTTP+SSE adapter so that other
banya CLIs (started with --mode remote) can talk to this host.

Endpoints:
  POST /api/v1/chat      ChatRequest JSON → SSE stream of ServerEvents
  POST /api/v1/approval  ApprovalResponse JSON → 200 OK
  GET  /api/v1/health    200 OK on sidecar ping`,
		RunE: runServe,
	}
	cmd.Flags().String("addr", ":8080", "Listen address for the HTTP server")
	cmd.Flags().String("token", "", "Bearer token clients must send in Authorization header (empty disables auth)")
	return cmd
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if sc, _ := cmd.Root().Flags().GetString("sidecar"); sc != "" {
		cfg.Sidecar.Path = sc
	}
	if u, _ := cmd.Root().Flags().GetString("llm-url"); u != "" {
		cfg.LLMServer.URL = u
	}
	if k, _ := cmd.Root().Flags().GetString("llm-key"); k != "" {
		cfg.LLMServer.APIKey = k
	}
	if m, _ := cmd.Root().Flags().GetString("llm-model"); m != "" {
		cfg.LLMServer.Model = m
	}

	addr, _ := cmd.Flags().GetString("addr")
	token, _ := cmd.Flags().GetString("token")

	pc, err := client.NewProcessClient(cfg.Sidecar.Path)
	if err != nil {
		return fmt.Errorf("init sidecar: %w", err)
	}
	defer pc.Close()
	pc.SetLLMBackend(client.NewLLMServerClient(
		cfg.LLMServer.URL, cfg.LLMServer.APIKey, cfg.LLMServer.Model,
	))

	if err := pc.HealthCheck(); err != nil {
		return fmt.Errorf("sidecar health check failed: %w", err)
	}

	srv := server.New(pc, server.Options{Addr: addr, BearerToken: token})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("banya serve listening on %s (sidecar=%s)\n", addr, pc.BinPath())
	if token == "" {
		fmt.Println("WARNING: --token is empty; the server will accept unauthenticated requests")
	}
	return srv.Run(ctx)
}

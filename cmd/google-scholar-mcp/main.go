package main

import (
	"context"
	"log"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/mcpserver"
)

var version = "0.1.0"

func main() {
	cfg := config.Load()
	server := mcpserver.New(cfg, version)

	if err := server.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

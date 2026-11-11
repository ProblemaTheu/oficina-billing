// Stub do Mercado Pago (API de Orders) para o compose e o BDD. Nunca em produção.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ProblemaTheu/oficina-billing/internal/infra/mercadopago"
)

func main() {
	porta := os.Getenv("PORTA")
	if porta == "" {
		porta = "8090"
	}
	base := os.Getenv("STUB_URL_PUBLICA")
	if base == "" {
		base = "http://localhost:" + porta
	}
	stub := mercadopago.NovoStub(base, os.Getenv("BILLING_WEBHOOK_URL"), os.Getenv("MP_WEBHOOK_SECRET"))
	srv := &http.Server{Addr: ":" + porta, Handler: stub, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("stub do Mercado Pago no ar", "porta", porta, "webhook", os.Getenv("BILLING_WEBHOOK_URL"))
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("stub parou", "error", err)
		os.Exit(1)
	}
}

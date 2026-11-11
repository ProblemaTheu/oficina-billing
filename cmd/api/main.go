// Billing Service: orçamento e pagamento pelo Mercado Pago (Fase 4).
//
// Um processo com quatro laços: API (cliente/funcionário + webhook do MP),
// consumidor de billing-commands.fifo, relay da outbox para os-saga-replies.fifo
// e o reconciliador de cobranças pendentes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/newrelic/go-agent/v3/integrations/logcontext-v2/nrslog"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/comandos"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/database"
	apihttp "github.com/ProblemaTheu/oficina-billing/internal/infra/http"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mercadopago"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/notificacao"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/repository"
)

type configuracao struct {
	FilaComandos, FilaRespostas            string
	MPBase, MPToken, MPSegredo, URLPublica string
	Porta                                  string
	Reconciliar                            time.Duration
	SegredoJWT                             []byte
}

func env(nome, padrao string) string {
	if v := os.Getenv(nome); v != "" {
		return v
	}
	return padrao
}

func lerConfiguracao() (configuracao, error) {
	c := configuracao{
		FilaComandos: os.Getenv("FILA_BILLING_URL"), FilaRespostas: os.Getenv("FILA_RESPOSTAS_URL"),
		MPBase: env("MP_BASE_URL", "https://api.mercadopago.com"), MPToken: os.Getenv("MP_ACCESS_TOKEN"),
		MPSegredo: os.Getenv("MP_WEBHOOK_SECRET"), URLPublica: env("URL_PUBLICA", "http://localhost:8081"),
		Porta: env("PORTA", "8080"),
	}
	var err error
	if c.Reconciliar, err = time.ParseDuration(env("MP_RECONCILIAR_INTERVALO", "10s")); err != nil {
		return c, fmt.Errorf("MP_RECONCILIAR_INTERVALO: %w", err)
	}
	var faltando []string
	for nome, v := range map[string]string{"FILA_BILLING_URL": c.FilaComandos, "FILA_RESPOSTAS_URL": c.FilaRespostas,
		"MP_ACCESS_TOKEN": c.MPToken, "MP_WEBHOOK_SECRET": c.MPSegredo} {
		if v == "" {
			faltando = append(faltando, nome)
		}
	}
	if len(faltando) > 0 {
		return c, fmt.Errorf("variáveis obrigatórias ausentes: %v", faltando)
	}
	c.SegredoJWT, err = apihttp.SegredoJWT()
	return c, err
}

func notificador() cobranca.Notificador {
	if os.Getenv("NOTIFIER") != "smtp" {
		return notificacao.Log{}
	}
	return notificacao.NovoSMTP(env("SMTP_HOST", "localhost"), env("SMTP_PORT", "1025"),
		env("EMAIL_FROM", "billing@oficina.local"), os.Getenv("SMTP_USUARIO"), os.Getenv("SMTP_SENHA"))
}

func main() {
	nrApp := novoAppNewRelic()
	configurarLog(nrApp)
	cfg, err := lerConfiguracao()
	if err != nil {
		slog.Error("configuração inválida", "error", err)
		os.Exit(1)
	}
	ctx, parar := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer parar()

	db := database.Connect()
	defer db.Close() //nolint:errcheck
	if err := database.RunMigrations(db); err != nil {
		slog.Error("falha ao executar migrations", "error", err)
		os.Exit(1)
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("configuração AWS", "error", err)
		os.Exit(1)
	}
	tracer := mensageria.TracerNewRelic{App: nrApp}
	outbox := repository.NovoOutboxRepository(db)
	svc := cobranca.Novo(cobranca.Dependencias{
		UoW: repository.NovaUnitOfWork(db), Inbox: repository.NovoInboxRepository(),
		Orcamentos: repository.NovoOrcamentoRepository(db), Pagamentos: repository.NovoPagamentoRepository(db),
		Respostas: repository.NovasRespostas(outbox, tracer), Gateway: mercadopago.Novo(cfg.MPBase, cfg.MPToken),
		Notificar: notificador(), URLPublica: cfg.URLPublica,
	})
	clienteSQS := sqs.NewFromConfig(awsCfg)
	consumidor := mensageria.NovoConsumer(clienteSQS, mensageria.ConfigConsumer{
		FilaURL: cfg.FilaComandos, Handlers: comandos.Handlers(svc), Tracer: tracer,
	})
	relay := mensageria.NovoRelay(outbox, mensageria.NovoPublisher(clienteSQS), mensageria.ConfigRelay{
		Destinos: map[string]string{repository.DestinoOS: cfg.FilaRespostas},
	})
	srv := &http.Server{
		Addr: ":" + cfg.Porta, ReadHeaderTimeout: 10 * time.Second,
		Handler: apihttp.Rotas(svc, apihttp.Config{SegredoJWT: cfg.SegredoJWT, SegredoWebhook: cfg.MPSegredo,
			Pronto: func(ctx context.Context) error { return db.PingContext(ctx) }}),
	}

	var wg sync.WaitGroup
	lacos := map[string]func(context.Context) error{
		"consumidor": consumidor.Executar, "relay": relay.Executar,
		"reconciliador": func(ctx context.Context) error { return reconciliar(ctx, svc, cfg.Reconciliar) },
	}
	for nome, laco := range lacos {
		wg.Add(1)
		go func(nome string, laco func(context.Context) error) {
			defer wg.Done()
			if err := laco(ctx); err != nil {
				slog.Error(nome+" parou", "error", err)
			}
		}(nome, laco)
	}
	go func() {
		slog.Info("billing service no ar", "porta", cfg.Porta, "mp_base", cfg.MPBase, "reconciliar", cfg.Reconciliar.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("servidor HTTP", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("sinal de término recebido; encerrando")
	desligar, cancelar := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelar()
	if err := srv.Shutdown(desligar); err != nil {
		slog.Warn("shutdown HTTP com erro", "error", err)
	}
	wg.Wait()
	slog.Info("encerrado")
}

// reconciliar confere as cobranças pendentes a cada intervalo — é o mecanismo
// principal de confirmação (no sandbox o MP não notifica orders de teste).
func reconciliar(ctx context.Context, svc *cobranca.Servico, intervalo time.Duration) error {
	slog.Info("reconciliador iniciado", "intervalo", intervalo.String())
	t := time.NewTicker(intervalo)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciliador encerrado")
			return nil
		case <-t.C:
			if _, err := svc.Reconciliar(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("ciclo de reconciliação falhou", "error", err)
			}
		}
	}
}

func novoAppNewRelic() *newrelic.Application {
	licenca := os.Getenv("NEW_RELIC_LICENSE_KEY")
	if licenca == "" {
		slog.Info("new relic desabilitado: NEW_RELIC_LICENSE_KEY ausente")
		return nil
	}
	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName(env("NEW_RELIC_APP_NAME", "oficina-billing")), newrelic.ConfigLicense(licenca),
		newrelic.ConfigDistributedTracerEnabled(true), newrelic.ConfigAppLogForwardingEnabled(true),
	)
	if err != nil {
		slog.Warn("new relic: falha ao inicializar", "error", err)
		return nil
	}
	return app
}

func configurarLog(nrApp *newrelic.Application) {
	nivel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		nivel = slog.LevelDebug
	}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: nivel,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	})
	if nrApp != nil {
		h = nrslog.WrapHandler(nrApp, h)
	}
	slog.SetDefault(slog.New(h).With("service.name", env("NEW_RELIC_APP_NAME", "oficina-billing"), "hostname", os.Getenv("HOSTNAME")))
}

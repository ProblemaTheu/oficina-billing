//go:build integration

// Integração do Billing contra PostgreSQL real (banco próprio do serviço):
//
//	./scripts/test-integration.sh
package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/database"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/repository"
)

var db *sql.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Println("TEST_DATABASE_URL não definido — testes de integração ignorados")
		return
	}
	var err error
	if db, err = sql.Open("postgres", dsn); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for i := 0; i < 30 && db.Ping() != nil; i++ {
		time.Sleep(time.Second)
	}
	if err := database.RunMigrations(db); err != nil {
		fmt.Println("migrations:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func novoOrcamento(t *testing.T) orcamento.Orcamento {
	t.Helper()
	o, err := orcamento.Novo(uuid.New(), uuid.New(), "OS-1", orcamento.Cliente{ID: uuid.New(), Nome: "Maria", Email: "m@x.com"}, "diag",
		[]orcamento.Item{{Tipo: "peca", Ref: "r1", Nome: "Filtro", Quantidade: 2, PrecoUnitario: 2890}, {Tipo: "servico", Nome: "Troca", Quantidade: 1, PrecoUnitario: 5000}},
		time.Now().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func emUoW(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return repository.NovaUnitOfWork(db).Executar(context.Background(), fn)
}

func TestIntegracaoOrcamento_CriarBuscarItensEmOrdem(t *testing.T) {
	r := repository.NovoOrcamentoRepository(db)
	o := novoOrcamento(t)
	if err := emUoW(t, func(ctx context.Context) error { return r.Criar(ctx, o) }); err != nil {
		t.Fatal(err)
	}
	lido, err := r.Buscar(context.Background(), o.ID)
	if err != nil || lido.Total != 10780 || lido.Status != orcamento.Gerado || lido.Cliente.ID != o.Cliente.ID || len(lido.Itens) != 2 ||
		lido.Itens[0].Nome != "Filtro" || lido.Itens[0].PrecoUnitario != 2890 || lido.Itens[1].Nome != "Troca" {
		t.Fatalf("round-trip: %+v %v", lido, err)
	}
	if porOS, err := r.BuscarPorOS(context.Background(), o.OsID); err != nil || porOS.ID != o.ID {
		t.Errorf("BuscarPorOS: %v", err)
	}
	if _, err := r.Buscar(context.Background(), uuid.New()); !errors.Is(err, cobranca.ErrNaoEncontrado) {
		t.Errorf("inexistente: %v", err)
	}
	// Um orçamento por OS: o banco barra o segundo.
	dup := novoOrcamento(t)
	dup.OsID = o.OsID
	if err := emUoW(t, func(ctx context.Context) error { return r.Criar(ctx, dup) }); err == nil {
		t.Error("segundo orçamento para a mesma OS deveria violar o UNIQUE")
	}
	if err := r.Criar(context.Background(), novoOrcamento(t)); !errors.Is(err, repository.ErrSemTransacao) {
		t.Errorf("criar fora de transação: %v", err)
	}
}

func TestIntegracaoOrcamento_MudarStatusCondicional(t *testing.T) {
	r := repository.NovoOrcamentoRepository(db)
	o := novoOrcamento(t)
	_ = emUoW(t, func(ctx context.Context) error { return r.Criar(ctx, o) })

	aprovado := o
	_ = aprovado.Aprovar(time.Now())
	if err := r.MudarStatus(context.Background(), aprovado, orcamento.Gerado); err != nil {
		t.Fatal(err)
	}
	// Recusa concorrente que leu o orçamento ainda "gerado".
	recusado := o
	_ = recusado.Recusar("caro", time.Now())
	if err := r.MudarStatus(context.Background(), recusado, orcamento.Gerado); !errors.Is(err, cobranca.ErrConflito) {
		t.Fatalf("esperava conflito, veio %v", err)
	}
	if lido, _ := r.Buscar(context.Background(), o.ID); lido.Status != orcamento.Aprovado {
		t.Errorf("status = %s", lido.Status)
	}
}

func TestIntegracaoPagamento_CicloEPendentes(t *testing.T) {
	ro, rp := repository.NovoOrcamentoRepository(db), repository.NovoPagamentoRepository(db)
	o := novoOrcamento(t)
	p := orcamento.Pagamento{ID: uuid.New(), OrcamentoID: o.ID, MPOrderID: "ORDTST-" + uuid.NewString()[:8], CheckoutURL: "http://mp/x",
		Status: orcamento.PagamentoPendente, Valor: o.Total, CriadoEm: time.Now(), AtualizadoEm: time.Now()}
	if err := emUoW(t, func(ctx context.Context) error {
		if err := ro.Criar(ctx, o); err != nil {
			return err
		}
		return rp.Criar(ctx, p)
	}); err != nil {
		t.Fatal(err)
	}

	pend, err := rp.Pendentes(context.Background(), 100)
	achou := false
	for _, x := range pend {
		achou = achou || x.ID == p.ID
	}
	if err != nil || !achou {
		t.Fatalf("pendente não listado: %v", err)
	}
	if porOrder, err := rp.BuscarPorOrder(context.Background(), p.MPOrderID); err != nil || porOrder.ID != p.ID || porOrder.Valor != o.Total {
		t.Errorf("BuscarPorOrder: %+v %v", porOrder, err)
	}

	aprovado := p
	aprovado.Status, aprovado.StatusDetail = orcamento.PagamentoAprovado, "accredited"
	if err := rp.MudarStatus(context.Background(), aprovado, orcamento.PagamentoPendente); err != nil {
		t.Fatal(err)
	}
	// Webhook e reconciliador na mesma order: o segundo encontra conflito.
	if err := rp.MudarStatus(context.Background(), aprovado, orcamento.PagamentoPendente); !errors.Is(err, cobranca.ErrConflito) {
		t.Errorf("segunda confirmação: %v", err)
	}
	estornado := aprovado
	estornado.Status, estornado.MPRefundID = orcamento.PagamentoEstornado, "REF1"
	_ = rp.MudarStatus(context.Background(), estornado, orcamento.PagamentoAprovado)
	lido, _ := rp.BuscarPorOrcamento(context.Background(), o.ID)
	if lido.Status != orcamento.PagamentoEstornado || lido.MPRefundID != "REF1" || lido.StatusDetail != "accredited" {
		t.Errorf("lido = %+v", lido)
	}
	if _, err := rp.BuscarPorOrder(context.Background(), "NAOEXISTE"); !errors.Is(err, cobranca.ErrNaoEncontrado) {
		t.Errorf("inexistente: %v", err)
	}
}

func TestIntegracaoRespostas_GravaEnvelopeNaOutbox(t *testing.T) {
	outbox := repository.NovoOutboxRepository(db)
	resp := repository.NovasRespostas(outbox, nil)
	saga, os := uuid.New(), uuid.New()
	ctx := logging.ComCorrelacao(context.Background(), "req-billing")
	if err := repository.NovaUnitOfWork(db).Executar(ctx, func(ctx context.Context) error {
		return resp.Responder(ctx, cobranca.Saida{Tipo: cobranca.TipoOrcamentoGerado, SagaID: saga, OsID: os, Payload: map[string]string{"valor_total": "57.80"}})
	}); err != nil {
		t.Fatal(err)
	}
	pend, _ := outbox.Pendentes(context.Background(), 1000)
	var env *mensageria.Envelope
	for _, m := range pend {
		if m.Envelope.OsID == os {
			env = &m.Envelope
			if m.Destino != repository.DestinoOS {
				t.Errorf("destino = %s", m.Destino)
			}
		}
	}
	if env == nil || env.Origem != mensageria.OrigemBilling || env.CorrelationID != "req-billing" || env.SagaID != saga {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestIntegracaoInbox(t *testing.T) {
	inbox := repository.NovoInboxRepository()
	id := uuid.New()
	var a, b bool
	_ = emUoW(t, func(ctx context.Context) (err error) { a, err = inbox.Marcar(ctx, id, "X"); return })
	_ = emUoW(t, func(ctx context.Context) (err error) { b, err = inbox.Marcar(ctx, id, "X"); return })
	if !a || b {
		t.Errorf("primeira=%v segunda=%v", a, b)
	}
}

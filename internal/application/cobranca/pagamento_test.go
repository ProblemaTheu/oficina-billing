package cobranca

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

func TestConfirmarCobranca_Processada(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	c.gw.estado = EstadoDaCobranca{Status: "processed", StatusDetail: "accredited", Pago: o.Total}

	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); err != nil {
		t.Fatal(err)
	}
	if c.pag.porID[p.ID].Status != orcamento.PagamentoAprovado {
		t.Errorf("pagamento = %s", c.pag.porID[p.ID].Status)
	}
	r := c.resp.saidas[0]
	if r.Tipo != TipoPagamentoConfirmado || r.Payload.(map[string]string)["valor"] != "179.80" || r.OsID != o.OsID {
		t.Errorf("PagamentoConfirmado = %+v", r)
	}
	// Webhook e reconciliador na mesma order: o segundo não faz nada.
	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); err != nil || len(c.resp.saidas) != 1 {
		t.Errorf("segunda confirmação: %v, respostas %d", err, len(c.resp.saidas))
	}
}

func TestConfirmarCobranca_TentativaRecusadaCancelaAntesDeRecusar(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	c.gw.estado = EstadoDaCobranca{Status: "action_required", StatusDetail: "rejected_by_issuer"}

	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); err != nil {
		t.Fatal(err)
	}
	if len(c.gw.canceladas) != 1 || c.gw.canceladas[0] != p.MPOrderID {
		t.Fatal("a order recusada deve ser cancelada (senão o cliente paga depois da compensação)")
	}
	r := c.resp.saidas[0]
	if c.pag.porID[p.ID].Status != orcamento.PagamentoRecusado || r.Tipo != TipoPagamentoRecusado ||
		r.Payload.(map[string]string)["mp_status_detail"] != "rejected_by_issuer" {
		t.Errorf("recusa: %s %+v", c.pag.porID[p.ID].Status, r)
	}
}

func TestConfirmarCobranca_CancelamentoFalhaNaoRecusa(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	c.gw.estado = EstadoDaCobranca{Status: "action_required"}
	c.gw.errCancelar = errFalha // ex.: o cliente acabou de pagar numa nova tentativa

	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); !errors.Is(err, errFalha) {
		t.Fatalf("esperava o erro do cancelamento: %v", err)
	}
	if c.pag.porID[p.ID].Status != orcamento.PagamentoPendente || len(c.resp.saidas) != 0 {
		t.Error("sem cancelar não recusa: o próximo ciclo decide")
	}
}

func TestConfirmarCobranca_OutrosStatus(t *testing.T) {
	casos := []struct {
		mp       string
		final    orcamento.StatusPagamento
		resposta string
	}{
		{"failed", orcamento.PagamentoRecusado, TipoPagamentoRecusado},
		{"canceled", orcamento.PagamentoRecusado, TipoPagamentoRecusado},
		{"expired", orcamento.PagamentoRecusado, TipoPagamentoRecusado},
		{"refunded", orcamento.PagamentoEstornado, ""},
		{"created", orcamento.PagamentoPendente, ""},
		{"processing", orcamento.PagamentoPendente, ""},
	}
	for _, k := range casos {
		t.Run(k.mp, func(t *testing.T) {
			c := novoCenario()
			o := c.comOrcamento(orcamento.Aprovado)
			p := c.comPagamento(o, orcamento.PagamentoPendente)
			c.gw.estado = EstadoDaCobranca{Status: k.mp}
			if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); err != nil {
				t.Fatal(err)
			}
			if c.pag.porID[p.ID].Status != k.final {
				t.Errorf("pagamento = %s, esperava %s", c.pag.porID[p.ID].Status, k.final)
			}
			if (k.resposta == "") != (len(c.resp.saidas) == 0) || (k.resposta != "" && c.resp.saidas[0].Tipo != k.resposta) {
				t.Errorf("respostas = %v", c.resp.tipos())
			}
		})
	}
}

// Pagou depois de a saga compensar: estorna e NÃO confirma (F4-2.5 passo 4).
func TestConfirmarCobranca_PagamentoTardioEstorna(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Cancelado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	c.gw.estado = EstadoDaCobranca{Status: "processed"}

	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); err != nil {
		t.Fatal(err)
	}
	if len(c.gw.estornos) != 1 || c.pag.porID[p.ID].Status != orcamento.PagamentoEstornado || c.pag.porID[p.ID].MPRefundID != "REF1" {
		t.Fatalf("estorno tardio: %v %+v", c.gw.estornos, c.pag.porID[p.ID])
	}
	if len(c.resp.saidas) != 0 {
		t.Errorf("pagamento tardio não confirma nem responde à saga: %v", c.resp.tipos())
	}
}

func TestConfirmarCobranca_Erros(t *testing.T) {
	c := novoCenario()
	if err := c.s.ConfirmarCobranca(context.Background(), "inexistente"); !errors.Is(err, ErrNaoEncontrado) {
		t.Errorf("order desconhecida: %v", err)
	}
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	c.gw.errConsultar = errFalha
	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); !errors.Is(err, errFalha) {
		t.Errorf("consulta: %v", err)
	}
	c.gw.errConsultar = nil
	c.gw.estado = EstadoDaCobranca{Status: "processed"}
	c.pag.errMudar = errFalha
	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); !errors.Is(err, errFalha) {
		t.Errorf("gravar: %v", err)
	}
	c.pag.errMudar = nil
	c.orc.errBuscar = errFalha
	if err := c.s.ConfirmarCobranca(context.Background(), p.MPOrderID); !errors.Is(err, errFalha) {
		t.Errorf("buscar orçamento: %v", err)
	}
}

func TestReconciliar_ProcessaPendentesMesmoComFalhaNumaDelas(t *testing.T) {
	c := novoCenario()
	for i := 0; i < 3; i++ {
		o := c.comOrcamento(orcamento.Aprovado)
		c.comPagamento(o, orcamento.PagamentoPendente)
	}
	c.gw.estado = EstadoDaCobranca{Status: "processed"}
	n, err := c.s.Reconciliar(context.Background())
	if err != nil || n != 3 || len(c.resp.saidas) != 3 {
		t.Fatalf("reconciliar: %d %v %d", n, err, len(c.resp.saidas))
	}
	c.pag.errBuscar = errFalha
	if _, err := c.s.Reconciliar(context.Background()); !errors.Is(err, errFalha) {
		t.Errorf("erro ao listar: %v", err)
	}

	f := novoCenario()
	f.comPagamento(f.comOrcamento(orcamento.Aprovado), orcamento.PagamentoPendente)
	f.gw.errConsultar = errFalha
	if n, err := f.s.Reconciliar(context.Background()); err != nil || n != 1 {
		t.Errorf("falha numa order não derruba o ciclo: %d %v", n, err)
	}
}

func TestEstornarPagamento(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoAprovado)
	k := Comando{MessageID: uuid.New(), Tipo: TipoEstornarPagamento, SagaID: o.SagaID, OsID: o.OsID}

	if err := c.s.EstornarPagamento(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if c.gw.chaves[0] != k.MessageID.String() || c.pag.porID[p.ID].Status != orcamento.PagamentoEstornado {
		t.Errorf("estorno: chave %v, status %s", c.gw.chaves, c.pag.porID[p.ID].Status)
	}
	r := c.resp.saidas[0]
	if r.Tipo != TipoPagamentoEstornado || r.Payload.(map[string]any)["refund_id"] != "REF1" {
		t.Errorf("PagamentoEstornado = %+v", r)
	}
	// Reentrega: inbox barra a segunda resposta.
	_ = c.s.EstornarPagamento(context.Background(), k)
	if len(c.resp.saidas) != 1 {
		t.Errorf("reentrega respondeu de novo: %v", c.resp.tipos())
	}
}

func TestEstornarPagamento_SemPagamentoAprovadoRespondeMesmoAssim(t *testing.T) {
	for nome, montar := range map[string]func(c cenario) Comando{
		"sem orçamento": func(c cenario) Comando { return cmd(TipoEstornarPagamento) },
		"sem pagamento": func(c cenario) Comando {
			o := c.comOrcamento(orcamento.Aprovado)
			return Comando{MessageID: uuid.New(), Tipo: TipoEstornarPagamento, SagaID: o.SagaID, OsID: o.OsID}
		},
		"pagamento recusado": func(c cenario) Comando {
			o := c.comOrcamento(orcamento.Aprovado)
			c.comPagamento(o, orcamento.PagamentoRecusado)
			return Comando{MessageID: uuid.New(), Tipo: TipoEstornarPagamento, SagaID: o.SagaID, OsID: o.OsID}
		},
	} {
		t.Run(nome, func(t *testing.T) {
			c := novoCenario()
			if err := c.s.EstornarPagamento(context.Background(), montar(c)); err != nil {
				t.Fatal(err)
			}
			if len(c.gw.estornos) != 0 || len(c.resp.saidas) != 1 || c.resp.saidas[0].Payload.(map[string]any)["refund_id"] != nil {
				t.Errorf("compensar o que não aconteceu é sucesso sem estorno: %v %v", c.gw.estornos, c.resp.saidas)
			}
		})
	}
}

func TestEstornarPagamento_Erros(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	c.comPagamento(o, orcamento.PagamentoAprovado)
	k := Comando{MessageID: uuid.New(), Tipo: TipoEstornarPagamento, SagaID: o.SagaID, OsID: o.OsID}
	c.gw.errEstornar = errFalha
	if err := c.s.EstornarPagamento(context.Background(), k); !errors.Is(err, errFalha) || len(c.resp.saidas) != 0 {
		t.Errorf("MP fora: erro volta para a fila (reentrega/DLQ): %v", err)
	}
	c.gw.errEstornar = ErrJaEstornado
	if err := c.s.EstornarPagamento(context.Background(), k); err != nil || len(c.resp.saidas) != 1 {
		t.Errorf("já estornado (409) é sucesso: %v", err)
	}
	b := novoCenario()
	b.orc.errBuscar = errFalha
	if err := b.s.EstornarPagamento(context.Background(), cmd(TipoEstornarPagamento)); !errors.Is(err, errFalha) {
		t.Errorf("buscar orçamento: %v", err)
	}
	p := novoCenario()
	op := p.comOrcamento(orcamento.Aprovado)
	p.pag.errBuscar = errFalha
	if err := p.s.EstornarPagamento(context.Background(), Comando{MessageID: uuid.New(), OsID: op.OsID}); !errors.Is(err, errFalha) {
		t.Errorf("buscar pagamento: %v", err)
	}
}

func TestCancelarOrcamento(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Aprovado)
	p := c.comPagamento(o, orcamento.PagamentoPendente)
	k := Comando{MessageID: uuid.New(), Tipo: TipoCancelarOrcamento, SagaID: o.SagaID, OsID: o.OsID}

	if err := c.s.CancelarOrcamento(context.Background(), k, " compensacao_T5 "); err != nil {
		t.Fatal(err)
	}
	if c.orc.porID[o.ID].Status != orcamento.Cancelado || c.orc.porID[o.ID].Motivo != "compensacao_T5" {
		t.Errorf("orçamento = %+v", c.orc.porID[o.ID])
	}
	if len(c.gw.canceladas) != 1 || c.pag.porID[p.ID].Status != orcamento.PagamentoCancelado {
		t.Errorf("a order pendente deve ser fechada e sair da reconciliação: %v %s", c.gw.canceladas, c.pag.porID[p.ID].Status)
	}
	if c.resp.saidas[0].Tipo != TipoOrcamentoCancelado {
		t.Errorf("resposta = %v", c.resp.tipos())
	}

	// Já cancelado: responde mesmo assim.
	k2 := k
	k2.MessageID = uuid.New()
	if err := c.s.CancelarOrcamento(context.Background(), k2, ""); err != nil || len(c.resp.saidas) != 2 {
		t.Errorf("cancelar de novo: %v %v", err, c.resp.tipos())
	}
}

func TestCancelarOrcamento_CasosDeBorda(t *testing.T) {
	c := novoCenario()
	if err := c.s.CancelarOrcamento(context.Background(), cmd(TipoCancelarOrcamento), ""); err != nil || c.resp.tipos()[0] != TipoOrcamentoCancelado {
		t.Errorf("sem orçamento responde: %v %v", err, c.resp.tipos())
	}
	// Falha ao cancelar a order (cliente pagou no meio): segue, a reconciliação estorna.
	f := novoCenario()
	o := f.comOrcamento(orcamento.Aprovado)
	f.comPagamento(o, orcamento.PagamentoPendente)
	f.gw.errCancelar = errFalha
	if err := f.s.CancelarOrcamento(context.Background(), Comando{MessageID: uuid.New(), SagaID: o.SagaID, OsID: o.OsID}, ""); err != nil {
		t.Errorf("falha no cancel da order não impede a compensação: %v", err)
	}
	e := novoCenario()
	e.orc.errBuscar = errFalha
	if err := e.s.CancelarOrcamento(context.Background(), cmd(TipoCancelarOrcamento), ""); !errors.Is(err, errFalha) {
		t.Errorf("buscar: %v", err)
	}
	g := novoCenario()
	og := g.comOrcamento(orcamento.Gerado)
	g.pag.errBuscar = errFalha
	if err := g.s.CancelarOrcamento(context.Background(), Comando{MessageID: uuid.New(), OsID: og.OsID}, ""); !errors.Is(err, errFalha) {
		t.Errorf("buscar pagamento: %v", err)
	}
	m := novoCenario()
	om := m.comOrcamento(orcamento.Gerado)
	m.orc.errMudar = errFalha
	if err := m.s.CancelarOrcamento(context.Background(), Comando{MessageID: uuid.New(), OsID: om.OsID}, ""); !errors.Is(err, errFalha) {
		t.Errorf("gravar: %v", err)
	}
}

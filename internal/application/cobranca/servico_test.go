package cobranca

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

func payloadGerar() PayloadGerarOrcamento {
	var p PayloadGerarOrcamento
	p.NumeroOS, p.Diagnostico = "OS-2026-00001", "amortecedor vazando"
	p.Cliente.ID, p.Cliente.Nome, p.Cliente.Email = uuid.NewString(), "Maria", " maria@exemplo.com "
	p.Itens = append(p.Itens, struct {
		Tipo  string `json:"tipo"`
		Ref   string `json:"ref"`
		Nome  string `json:"nome"`
		Qtd   int    `json:"qtd"`
		Preco string `json:"preco"`
	}{"peca", "ref", "Filtro", 2, "28.90"})
	return p
}

func cmd(tipo string) Comando {
	return Comando{MessageID: uuid.New(), Tipo: tipo, SagaID: uuid.New(), OsID: uuid.New()}
}

func TestGerarOrcamento_CriaRespondeEEnviaEmail(t *testing.T) {
	c := novoCenario()
	k := cmd(TipoGerarOrcamento)

	if err := c.s.GerarOrcamento(context.Background(), k, payloadGerar()); err != nil {
		t.Fatal(err)
	}
	o, err := c.orc.BuscarPorOS(context.Background(), k.OsID)
	if err != nil || o.Total != 5780 || o.Status != orcamento.Gerado || o.Cliente.Email != "maria@exemplo.com" {
		t.Fatalf("orçamento = %+v (%v)", o, err)
	}
	if len(c.resp.saidas) != 1 || c.resp.saidas[0].Tipo != TipoOrcamentoGerado {
		t.Fatalf("respostas = %v", c.resp.tipos())
	}
	p := c.resp.saidas[0].Payload.(map[string]string)
	if p["orcamento_id"] != o.ID.String() || p["valor_total"] != "57.80" {
		t.Errorf("OrcamentoGerado = %v", p)
	}
	if c.notif.para != "maria@exemplo.com" || !strings.Contains(c.notif.corpo, "R$ 57.80") ||
		!strings.Contains(c.notif.corpo, "http://billing/billing/v1/orcamentos/"+o.ID.String()) {
		t.Errorf("e-mail = %+v", c.notif)
	}
}

func TestGerarOrcamento_Idempotencia(t *testing.T) {
	c := novoCenario()
	k := cmd(TipoGerarOrcamento)
	_ = c.s.GerarOrcamento(context.Background(), k, payloadGerar())

	// Mesma mensagem: inbox barra.
	_ = c.s.GerarOrcamento(context.Background(), k, payloadGerar())
	// Outra mensagem para a mesma OS (saga repetindo): mesmo orcamento_id.
	k2 := k
	k2.MessageID = uuid.New()
	_ = c.s.GerarOrcamento(context.Background(), k2, payloadGerar())

	if len(c.orc.porID) != 1 || len(c.resp.saidas) != 2 {
		t.Fatalf("orçamentos = %d, respostas = %d", len(c.orc.porID), len(c.resp.saidas))
	}
	a, b := c.resp.saidas[0].Payload.(map[string]string), c.resp.saidas[1].Payload.(map[string]string)
	if a["orcamento_id"] != b["orcamento_id"] {
		t.Error("a mesma OS deve receber o mesmo orçamento")
	}
}

func TestGerarOrcamento_ItemInvalidoRespondeFalha(t *testing.T) {
	c := novoCenario()
	p := payloadGerar()
	p.Itens[0].Preco = "28,90"
	if err := c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), p); err != nil {
		t.Fatal(err)
	}
	if len(c.orc.porID) != 0 || c.resp.tipos()[0] != TipoOrcamentoFalhou {
		t.Errorf("preço inválido deve responder OrcamentoFalhou: %v", c.resp.tipos())
	}
	sem := payloadGerar()
	sem.Itens = nil
	_ = c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), sem)
	if c.resp.tipos()[1] != TipoOrcamentoFalhou {
		t.Error("sem itens também é falha")
	}
}

func TestGerarOrcamento_Erros(t *testing.T) {
	c := novoCenario()
	c.inbox.err = errFalha
	if err := c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), payloadGerar()); !errors.Is(err, errFalha) {
		t.Errorf("inbox: %v", err)
	}
	c = novoCenario()
	c.orc.errBuscar = errFalha
	if err := c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), payloadGerar()); !errors.Is(err, errFalha) {
		t.Errorf("busca: %v", err)
	}
	c = novoCenario()
	c.orc.errCriar = errFalha
	if err := c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), payloadGerar()); !errors.Is(err, errFalha) {
		t.Errorf("criar: %v", err)
	}
	// Falha no e-mail não falha o comando (já commitado).
	c = novoCenario()
	c.notif.err = errFalha
	if err := c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), payloadGerar()); err != nil {
		t.Errorf("e-mail: %v", err)
	}
	// Sem e-mail do cliente: não tenta enviar.
	c = novoCenario()
	p := payloadGerar()
	p.Cliente.Email = ""
	_ = c.s.GerarOrcamento(context.Background(), cmd(TipoGerarOrcamento), p)
	if c.notif.para != "" {
		t.Error("sem e-mail não envia")
	}
}

func TestAprovar_CriaCobrancaERespondeComLink(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Gerado)

	o2, p, err := c.s.Aprovar(context.Background(), o.ID, Ator{Tipo: "cliente", Sub: o.Cliente.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if o2.Status != orcamento.Aprovado || p.Status != orcamento.PagamentoPendente || p.MPOrderID != "ORDTST1" || p.Valor != o.Total {
		t.Errorf("aprovar = %+v / %+v", o2, p)
	}
	if c.gw.criadas[0] != "179.80" {
		t.Errorf("valor cobrado = %v", c.gw.criadas)
	}
	r := c.resp.saidas[0]
	if r.Tipo != TipoOrcamentoAprovado || r.Payload.(map[string]string)["link_pagamento"] != "http://mp/checkout/ORDTST1" {
		t.Errorf("OrcamentoAprovado = %+v", r)
	}

	// Segundo clique: mesmo pagamento, nenhuma cobrança nova.
	_, p2, err := c.s.Aprovar(context.Background(), o.ID, Ator{Tipo: "usuario"})
	if err != nil || p2.ID != p.ID || len(c.gw.criadas) != 1 {
		t.Errorf("reaprovar: %v %v %d", err, p2.ID == p.ID, len(c.gw.criadas))
	}
}

func TestAprovar_Erros(t *testing.T) {
	c := novoCenario()
	if _, _, err := c.s.Aprovar(context.Background(), uuid.New(), Ator{}); !errors.Is(err, ErrNaoEncontrado) {
		t.Errorf("inexistente: %v", err)
	}
	o := c.comOrcamento(orcamento.Gerado)
	if _, _, err := c.s.Aprovar(context.Background(), o.ID, Ator{Tipo: "cliente", Sub: uuid.NewString()}); !errors.Is(err, ErrProibido) {
		t.Errorf("cliente de outra OS: %v", err)
	}
	r := c.comOrcamento(orcamento.Recusado)
	if _, _, err := c.s.Aprovar(context.Background(), r.ID, Ator{}); !errors.Is(err, orcamento.ErrEstadoInvalido) {
		t.Errorf("recusado: %v", err)
	}
	c.gw.errCriar = errFalha
	if _, _, err := c.s.Aprovar(context.Background(), o.ID, Ator{}); !errors.Is(err, errFalha) || c.orc.porID[o.ID].Status != orcamento.Gerado {
		t.Errorf("MP fora: nada gravado, pode tentar de novo: %v %s", err, c.orc.porID[o.ID].Status)
	}
	c.gw.errCriar = nil
	c.pag.errCriar = errFalha
	if _, _, err := c.s.Aprovar(context.Background(), o.ID, Ator{}); !errors.Is(err, errFalha) {
		t.Errorf("gravar pagamento: %v", err)
	}
}

func TestRecusar(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Gerado)
	if _, err := c.s.Recusar(context.Background(), o.ID, Ator{Tipo: "cliente", Sub: uuid.NewString()}, "x"); !errors.Is(err, ErrProibido) {
		t.Errorf("cliente de outra OS: %v", err)
	}
	r, err := c.s.Recusar(context.Background(), o.ID, Ator{Tipo: "cliente", Sub: o.Cliente.ID.String()}, " caro demais ")
	if err != nil || r.Status != orcamento.Recusado || r.Motivo != "caro demais" {
		t.Fatalf("recusar: %v %+v", err, r)
	}
	if c.resp.saidas[0].Tipo != TipoOrcamentoRecusado {
		t.Errorf("resposta = %v", c.resp.tipos())
	}
	if _, err := c.s.Recusar(context.Background(), o.ID, Ator{}, ""); !errors.Is(err, orcamento.ErrEstadoInvalido) {
		t.Errorf("recusar de novo: %v", err)
	}
	if _, err := c.s.Recusar(context.Background(), uuid.New(), Ator{}, ""); !errors.Is(err, ErrNaoEncontrado) {
		t.Errorf("inexistente: %v", err)
	}
}

func TestConsultar(t *testing.T) {
	c := novoCenario()
	o := c.comOrcamento(orcamento.Gerado)
	if _, p, err := c.s.Consultar(context.Background(), o.ID, Ator{}); err != nil || p != nil {
		t.Errorf("sem pagamento: %v %v", err, p)
	}
	pg := c.comPagamento(o, orcamento.PagamentoPendente)
	if _, p, err := c.s.ConsultarPorOS(context.Background(), o.OsID, Ator{}); err != nil || p == nil || p.ID != pg.ID {
		t.Errorf("com pagamento: %v %v", err, p)
	}
	if _, _, err := c.s.Consultar(context.Background(), o.ID, Ator{Tipo: "cliente", Sub: "outro"}); !errors.Is(err, ErrProibido) {
		t.Errorf("cliente de outra OS: %v", err)
	}
	if _, _, err := c.s.ConsultarPorOS(context.Background(), uuid.New(), Ator{}); !errors.Is(err, ErrNaoEncontrado) {
		t.Errorf("OS sem orçamento: %v", err)
	}
	if _, _, err := c.s.Consultar(context.Background(), uuid.New(), Ator{}); !errors.Is(err, ErrNaoEncontrado) {
		t.Errorf("inexistente: %v", err)
	}
	c.pag.errBuscar = errFalha
	if _, _, err := c.s.Consultar(context.Background(), o.ID, Ator{}); !errors.Is(err, errFalha) {
		t.Errorf("erro do repositório: %v", err)
	}
}

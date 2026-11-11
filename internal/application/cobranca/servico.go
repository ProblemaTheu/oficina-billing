package cobranca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// Dependencias do serviço.
type Dependencias struct {
	UoW        UnidadeDeTrabalho
	Inbox      Inbox
	Orcamentos Orcamentos
	Pagamentos Pagamentos
	Respostas  Respostas
	Gateway    Gateway
	Notificar  Notificador // opcional
	// URLPublica é a base dos links enviados ao cliente (ex.: http://localhost:8081).
	URLPublica string
}

// Servico reúne os casos de uso.
type Servico struct {
	d     Dependencias
	agora func() time.Time
}

// Novo cria o serviço.
func Novo(d Dependencias) *Servico { return &Servico{d: d, agora: time.Now} }

// ── GerarOrcamento (comando da saga) ──────────────────────────────────────────

// PayloadGerarOrcamento é o comando do orquestrador — autossuficiente.
type PayloadGerarOrcamento struct {
	NumeroOS    string `json:"numero_os"`
	Diagnostico string `json:"diagnostico"`
	Cliente     struct {
		ID    string `json:"id"`
		Nome  string `json:"nome"`
		Email string `json:"email"`
	} `json:"cliente"`
	Itens []struct {
		Tipo  string `json:"tipo"`
		Ref   string `json:"ref"`
		Nome  string `json:"nome"`
		Qtd   int    `json:"qtd"`
		Preco string `json:"preco"`
	} `json:"itens"`
}

func (p PayloadGerarOrcamento) orcamento(cmd Comando, agora time.Time) (orcamento.Orcamento, error) {
	itens := make([]orcamento.Item, 0, len(p.Itens))
	for _, i := range p.Itens {
		preco, err := orcamento.ParseCentavos(i.Preco)
		if err != nil {
			return orcamento.Orcamento{}, err
		}
		itens = append(itens, orcamento.Item{Tipo: i.Tipo, Ref: i.Ref, Nome: i.Nome, Quantidade: i.Qtd, PrecoUnitario: preco})
	}
	cliID, _ := uuid.Parse(p.Cliente.ID)
	return orcamento.Novo(cmd.OsID, cmd.SagaID, p.NumeroOS,
		orcamento.Cliente{ID: cliID, Nome: p.Cliente.Nome, Email: strings.TrimSpace(p.Cliente.Email)}, p.Diagnostico, itens, agora)
}

func respostaGerado(o orcamento.Orcamento) Saida {
	return Saida{Tipo: TipoOrcamentoGerado, SagaID: o.SagaID, OsID: o.OsID,
		Payload: map[string]string{"orcamento_id": o.ID.String(), "valor_total": o.Total.String()}}
}

// GerarOrcamento cria o orçamento da OS e responde OrcamentoGerado.
//
// Um orçamento por OS: se já existir (reentrega depois da janela do FIFO, ou
// saga repetindo), reenvia OrcamentoGerado com o MESMO orcamento_id —
// idempotência de negócio, além da inbox. Itens inválidos respondem
// OrcamentoFalhou (a saga compensa).
func (s *Servico) GerarOrcamento(ctx context.Context, cmd Comando, p PayloadGerarOrcamento) error {
	var (
		criado orcamento.Orcamento
		novo   bool
	)
	err := s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if ok, err := s.d.Inbox.Marcar(ctx, cmd.MessageID, cmd.Tipo); err != nil || !ok {
			return err
		}
		existente, err := s.d.Orcamentos.BuscarPorOS(ctx, cmd.OsID)
		if err == nil {
			return s.d.Respostas.Responder(ctx, respostaGerado(existente))
		}
		if !errors.Is(err, ErrNaoEncontrado) {
			return err
		}
		o, err := p.orcamento(cmd, s.agora())
		if err != nil {
			logging.Log(ctx).Warn("GerarOrcamento inválido; respondendo falha", "error", err)
			return s.d.Respostas.Responder(ctx, Saida{Tipo: TipoOrcamentoFalhou, SagaID: cmd.SagaID, OsID: cmd.OsID,
				Payload: map[string]string{"motivo": err.Error()}})
		}
		if err := s.d.Orcamentos.Criar(ctx, o); err != nil {
			return err
		}
		criado, novo = o, true
		return s.d.Respostas.Responder(ctx, respostaGerado(o))
	})
	if err != nil || !novo {
		return err
	}
	logging.Log(ctx).Info("orçamento gerado", "orcamento_id", criado.ID, "os_id", criado.OsID, "total", criado.Total.String())
	s.enviarOrcamento(ctx, criado) // depois do commit: e-mail não tem rollback
	return nil
}

func (s *Servico) enviarOrcamento(ctx context.Context, o orcamento.Orcamento) {
	if s.d.Notificar == nil || o.Cliente.Email == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Olá, %s!\n\nO orçamento da sua ordem de serviço %s está pronto.\n\n", o.Cliente.Nome, o.NumeroOS)
	if o.Diagnostico != "" {
		fmt.Fprintf(&b, "Diagnóstico: %s\n\n", o.Diagnostico)
	}
	for _, i := range o.Itens {
		fmt.Fprintf(&b, "- %s × %d: R$ %s\n", i.Nome, i.Quantidade, i.Subtotal().String())
	}
	fmt.Fprintf(&b, "\nTotal: R$ %s\n\nVeja e aprove: %s/billing/v1/orcamentos/%s\n", o.Total.String(), s.d.URLPublica, o.ID)
	if err := s.d.Notificar.Enviar(ctx, o.Cliente.Email, "Orçamento da OS "+o.NumeroOS, b.String()); err != nil {
		logging.Log(ctx).Warn("falha ao enviar o orçamento por e-mail", "orcamento_id", o.ID, "error", err)
	}
}

// ── Aprovar / recusar (cliente ou funcionário) ───────────────────────────────

// Aprovar cria a cobrança no Mercado Pago e responde OrcamentoAprovado com o
// link de pagamento. A cobrança é criada FORA da transação (chamada externa);
// a idempotency key = orcamento_id faz uma nova tentativa devolver a mesma.
// Se o Mercado Pago falhar, nada é gravado e o cliente pode tentar de novo.
func (s *Servico) Aprovar(ctx context.Context, id uuid.UUID, ator Ator) (orcamento.Orcamento, orcamento.Pagamento, error) {
	o, err := s.d.Orcamentos.Buscar(ctx, id)
	if err != nil {
		return o, orcamento.Pagamento{}, err
	}
	if !ator.podeDecidir(o) {
		return o, orcamento.Pagamento{}, ErrProibido
	}
	if o.Status == orcamento.Aprovado { // repetição do clique: devolve o mesmo link
		p, err := s.d.Pagamentos.BuscarPorOrcamento(ctx, o.ID)
		return o, p, err
	}
	anterior := o.Status
	if err := o.Aprovar(s.agora()); err != nil {
		return o, orcamento.Pagamento{}, err
	}
	cob, err := s.d.Gateway.CriarCobranca(ctx, NovaCobranca{OrcamentoID: o.ID, NumeroOS: o.NumeroOS, Valor: o.Total, EmailPagador: o.Cliente.Email})
	if err != nil {
		return o, orcamento.Pagamento{}, fmt.Errorf("criar cobrança: %w", err)
	}
	p := orcamento.Pagamento{ID: uuid.New(), OrcamentoID: o.ID, MPOrderID: cob.OrderID, CheckoutURL: cob.CheckoutURL,
		Status: orcamento.PagamentoPendente, Valor: o.Total, CriadoEm: s.agora().UTC(), AtualizadoEm: s.agora().UTC()}
	err = s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if err := s.d.Orcamentos.MudarStatus(ctx, o, anterior); err != nil {
			return err
		}
		if err := s.d.Pagamentos.Criar(ctx, p); err != nil {
			return err
		}
		return s.d.Respostas.Responder(ctx, Saida{Tipo: TipoOrcamentoAprovado, SagaID: o.SagaID, OsID: o.OsID,
			Payload: map[string]string{"orcamento_id": o.ID.String(), "link_pagamento": cob.CheckoutURL}})
	})
	if err != nil {
		return o, orcamento.Pagamento{}, err
	}
	logging.Log(ctx).Info("orçamento aprovado; cobrança criada", "orcamento_id", o.ID, "mp_order_id", cob.OrderID)
	return o, p, nil
}

// Recusar encerra o orçamento e responde OrcamentoRecusado (a saga compensa).
func (s *Servico) Recusar(ctx context.Context, id uuid.UUID, ator Ator, motivo string) (orcamento.Orcamento, error) {
	o, err := s.d.Orcamentos.Buscar(ctx, id)
	if err != nil {
		return o, err
	}
	if !ator.podeDecidir(o) {
		return o, ErrProibido
	}
	anterior := o.Status
	if err := o.Recusar(strings.TrimSpace(motivo), s.agora()); err != nil {
		return o, err
	}
	err = s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if err := s.d.Orcamentos.MudarStatus(ctx, o, anterior); err != nil {
			return err
		}
		return s.d.Respostas.Responder(ctx, Saida{Tipo: TipoOrcamentoRecusado, SagaID: o.SagaID, OsID: o.OsID,
			Payload: map[string]string{"orcamento_id": o.ID.String(), "motivo": o.Motivo}})
	})
	return o, err
}

// Consultar devolve o orçamento e, se houver, o pagamento.
func (s *Servico) Consultar(ctx context.Context, id uuid.UUID, ator Ator) (orcamento.Orcamento, *orcamento.Pagamento, error) {
	o, err := s.d.Orcamentos.Buscar(ctx, id)
	if err != nil {
		return o, nil, err
	}
	return s.comPagamento(ctx, o, ator)
}

// ConsultarPorOS devolve o orçamento de uma OS.
func (s *Servico) ConsultarPorOS(ctx context.Context, osID uuid.UUID, ator Ator) (orcamento.Orcamento, *orcamento.Pagamento, error) {
	o, err := s.d.Orcamentos.BuscarPorOS(ctx, osID)
	if err != nil {
		return o, nil, err
	}
	return s.comPagamento(ctx, o, ator)
}

func (s *Servico) comPagamento(ctx context.Context, o orcamento.Orcamento, ator Ator) (orcamento.Orcamento, *orcamento.Pagamento, error) {
	if !ator.podeDecidir(o) {
		return o, nil, ErrProibido
	}
	p, err := s.d.Pagamentos.BuscarPorOrcamento(ctx, o.ID)
	if errors.Is(err, ErrNaoEncontrado) {
		return o, nil, nil
	}
	if err != nil {
		return o, nil, err
	}
	return o, &p, nil
}

package orcamento

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status do orçamento.
type Status string

const (
	Gerado    Status = "gerado"
	Aprovado  Status = "aprovado"
	Recusado  Status = "recusado"
	Cancelado Status = "cancelado"
)

// StatusPagamento do pagamento (uma cobrança no Mercado Pago).
type StatusPagamento string

const (
	PagamentoPendente  StatusPagamento = "pendente"
	PagamentoAprovado  StatusPagamento = "aprovado"
	PagamentoRecusado  StatusPagamento = "recusado"
	PagamentoEstornado StatusPagamento = "estornado"
	PagamentoCancelado StatusPagamento = "cancelado"
)

// Erros de regra de negócio.
var (
	ErrEstadoInvalido = errors.New("operação inválida para o status do orçamento")
	ErrSemItens       = errors.New("orçamento sem itens")
	ErrItemInvalido   = errors.New("item de orçamento inválido")
)

// Item é uma linha do orçamento (serviço ou peça da OS).
type Item struct {
	Tipo          string
	Ref           string
	Nome          string
	Quantidade    int
	PrecoUnitario Centavos
}

// Subtotal do item.
func (i Item) Subtotal() Centavos { return i.PrecoUnitario * Centavos(i.Quantidade) }

// Cliente é o recorte do cliente que viaja no comando GerarOrcamento.
type Cliente struct {
	ID    uuid.UUID
	Nome  string
	Email string
}

// Orcamento de uma OS.
type Orcamento struct {
	ID           uuid.UUID
	OsID         uuid.UUID
	SagaID       uuid.UUID
	NumeroOS     string
	Cliente      Cliente
	Status       Status
	Diagnostico  string
	Itens        []Item
	Total        Centavos
	Motivo       string
	CriadoEm     time.Time
	AtualizadoEm time.Time
}

// Novo monta o orçamento a partir dos itens da OS.
func Novo(osID, sagaID uuid.UUID, numeroOS string, cli Cliente, diagnostico string, itens []Item, agora time.Time) (Orcamento, error) {
	if osID == uuid.Nil || sagaID == uuid.Nil {
		return Orcamento{}, errors.New("os_id e saga_id são obrigatórios")
	}
	if len(itens) == 0 {
		return Orcamento{}, ErrSemItens
	}
	var total Centavos
	for _, i := range itens {
		if i.Quantidade <= 0 || i.PrecoUnitario < 0 || strings.TrimSpace(i.Nome) == "" {
			return Orcamento{}, fmt.Errorf("%w: %+v", ErrItemInvalido, i)
		}
		total += i.Subtotal()
	}
	return Orcamento{
		ID: uuid.New(), OsID: osID, SagaID: sagaID, NumeroOS: numeroOS, Cliente: cli, Status: Gerado,
		Diagnostico: diagnostico, Itens: itens, Total: total, CriadoEm: agora.UTC(), AtualizadoEm: agora.UTC(),
	}, nil
}

func (o *Orcamento) mudar(para Status, motivo string, agora time.Time, permitidos ...Status) error {
	for _, p := range permitidos {
		if o.Status == p {
			o.Status, o.Motivo, o.AtualizadoEm = para, motivo, agora.UTC()
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", ErrEstadoInvalido, o.Status, para)
}

// Aprovar: só um orçamento gerado pode ser aprovado.
func (o *Orcamento) Aprovar(agora time.Time) error { return o.mudar(Aprovado, "", agora, Gerado) }

// Recusar: só um orçamento gerado pode ser recusado.
func (o *Orcamento) Recusar(motivo string, agora time.Time) error {
	return o.mudar(Recusado, motivo, agora, Gerado)
}

// Cancelar (compensação da saga): qualquer status não cancelado.
func (o *Orcamento) Cancelar(motivo string, agora time.Time) error {
	if o.Status == Cancelado {
		return nil // idempotente
	}
	return o.mudar(Cancelado, motivo, agora, Gerado, Aprovado, Recusado)
}

// Pagamento é a cobrança de um orçamento aprovado (uma order no Mercado Pago).
type Pagamento struct {
	ID           uuid.UUID
	OrcamentoID  uuid.UUID
	MPOrderID    string
	CheckoutURL  string
	Status       StatusPagamento
	Valor        Centavos
	StatusDetail string
	MPRefundID   string
	CriadoEm     time.Time
	AtualizadoEm time.Time
}

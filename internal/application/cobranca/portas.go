// Package cobranca contém os casos de uso do Billing Service: orçamento
// (gerar, aprovar, recusar, cancelar) e pagamento pelo Mercado Pago (cobrar,
// confirmar, estornar), respondendo ao orquestrador do OS Service pela outbox.
package cobranca

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// Tipos de mensagem do catálogo (arquitetura §5.2 do oficina-app).
const (
	TipoGerarOrcamento      = "GerarOrcamento"
	TipoOrcamentoGerado     = "OrcamentoGerado"
	TipoOrcamentoFalhou     = "OrcamentoFalhou"
	TipoOrcamentoAprovado   = "OrcamentoAprovado"
	TipoOrcamentoRecusado   = "OrcamentoRecusado"
	TipoPagamentoConfirmado = "PagamentoConfirmado"
	TipoPagamentoRecusado   = "PagamentoRecusado"
	TipoEstornarPagamento   = "EstornarPagamento"
	TipoPagamentoEstornado  = "PagamentoEstornado"
	TipoCancelarOrcamento   = "CancelarOrcamento"
	TipoOrcamentoCancelado  = "OrcamentoCancelado"
)

// Erros traduzidos pela API e pelo consumidor.
var (
	ErrNaoEncontrado = errors.New("não encontrado")
	ErrConflito      = errors.New("alterado por outra operação")
	ErrProibido      = errors.New("operação não permitida para este usuário")
	// ErrJaEstornado: o Mercado Pago responde 409 order_already_refunded —
	// para a saga é sucesso (o dinheiro já voltou).
	ErrJaEstornado = errors.New("cobrança já estornada")
)

// Comando identifica uma mensagem recebida da saga (para a inbox).
type Comando struct {
	MessageID uuid.UUID
	Tipo      string
	SagaID    uuid.UUID
	OsID      uuid.UUID
}

// Saida é uma resposta ao orquestrador, gravada na outbox na transação.
type Saida struct {
	Tipo    string
	SagaID  uuid.UUID
	OsID    uuid.UUID
	Payload any
}

// UnidadeDeTrabalho executa fn numa transação (a tx viaja no contexto).
type UnidadeDeTrabalho interface {
	Executar(ctx context.Context, fn func(ctx context.Context) error) error
}

// Inbox: false = mensagem já processada.
type Inbox interface {
	Marcar(ctx context.Context, messageID uuid.UUID, tipo string) (bool, error)
}

// Orcamentos persiste orçamentos e itens.
type Orcamentos interface {
	Criar(ctx context.Context, o orcamento.Orcamento) error
	Buscar(ctx context.Context, id uuid.UUID) (orcamento.Orcamento, error)
	BuscarPorOS(ctx context.Context, osID uuid.UUID) (orcamento.Orcamento, error)
	// MudarStatus grava status/motivo se o status no banco ainda for `anterior`.
	MudarStatus(ctx context.Context, o orcamento.Orcamento, anterior orcamento.Status) error
}

// Pagamentos persiste as cobranças.
type Pagamentos interface {
	Criar(ctx context.Context, p orcamento.Pagamento) error
	BuscarPorOrcamento(ctx context.Context, orcamentoID uuid.UUID) (orcamento.Pagamento, error)
	BuscarPorOrder(ctx context.Context, orderID string) (orcamento.Pagamento, error)
	MudarStatus(ctx context.Context, p orcamento.Pagamento, anterior orcamento.StatusPagamento) error
	Pendentes(ctx context.Context, limite int) ([]orcamento.Pagamento, error)
}

// Respostas grava as respostas à saga na outbox (transação do contexto).
type Respostas interface {
	Responder(ctx context.Context, s Saida) error
}

// NovaCobranca é o pedido de cobrança ao gateway.
type NovaCobranca struct {
	OrcamentoID  uuid.UUID
	NumeroOS     string
	Valor        orcamento.Centavos
	EmailPagador string
}

// Cobranca é a cobrança criada (order do Mercado Pago).
type Cobranca struct {
	OrderID     string
	CheckoutURL string
}

// EstadoDaCobranca é o que o gateway diz da cobrança — a verdade vem daqui,
// nunca do corpo do webhook.
type EstadoDaCobranca struct {
	Status       string // created, processing, action_required, processed, failed, canceled, expired, refunded
	StatusDetail string
	Pago         orcamento.Centavos
}

// Gateway é o meio de pagamento (Mercado Pago, API de Orders).
type Gateway interface {
	CriarCobranca(ctx context.Context, c NovaCobranca) (Cobranca, error)
	Consultar(ctx context.Context, orderID string) (EstadoDaCobranca, error)
	Cancelar(ctx context.Context, orderID string) error
	// Estornar devolve o id do estorno; ErrJaEstornado se já estava estornada.
	Estornar(ctx context.Context, orderID, chaveIdempotencia string) (string, error)
}

// Notificador envia o orçamento ao cliente.
type Notificador interface {
	Enviar(ctx context.Context, para, assunto, corpo string) error
}

// Ator é quem chama a API (claims do JWT).
type Ator struct {
	Tipo string // "cliente" | "usuario"
	Sub  string
}

// funcionario: tokens sem tipo são de funcionário (regra de transição do OS).
func (a Ator) funcionario() bool { return a.Tipo == "" || a.Tipo == "usuario" }

// podeDecidir: cliente só aprova/recusa o próprio orçamento; funcionário, qualquer.
func (a Ator) podeDecidir(o orcamento.Orcamento) bool {
	return a.funcionario() || (a.Tipo == "cliente" && a.Sub == o.Cliente.ID.String())
}
